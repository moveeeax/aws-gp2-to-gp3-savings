package finder

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	ec2types "github.com/aws/aws-sdk-go-v2/service/ec2/types"
)

// mockEC2 is an injected EC2API for deterministic, offline tests. It paginates
// one volume per page so the pagination loop is exercised.
type mockEC2 struct {
	vols []ec2types.Volume
}

func (m *mockEC2) DescribeVolumes(_ context.Context, in *ec2.DescribeVolumesInput, _ ...func(*ec2.Options)) (*ec2.DescribeVolumesOutput, error) {
	start := 0
	if in.NextToken != nil {
		// token is the index of the next volume, encoded as a decimal string.
		for _, c := range *in.NextToken {
			start = start*10 + int(c-'0')
		}
	}
	if start >= len(m.vols) {
		return &ec2.DescribeVolumesOutput{}, nil
	}
	out := &ec2.DescribeVolumesOutput{Volumes: []ec2types.Volume{m.vols[start]}}
	if start+1 < len(m.vols) {
		next := itoa(start + 1)
		out.NextToken = aws.String(next)
	}
	return out, nil
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	return string(b)
}

func vol(id, vtype, az string, size int32, inst string) ec2types.Volume {
	v := ec2types.Volume{
		VolumeId:         aws.String(id),
		VolumeType:       ec2types.VolumeType(vtype),
		AvailabilityZone: aws.String(az),
		Size:             aws.Int32(size),
		State:            ec2types.VolumeStateAvailable,
	}
	if inst != "" {
		v.State = ec2types.VolumeStateInUse
		v.Attachments = []ec2types.VolumeAttachment{{InstanceId: aws.String(inst)}}
	}
	return v
}

func TestClassify(t *testing.T) {
	gp2GB, gp3GB := Prices("us-east-1")
	tests := []struct {
		name     string
		v        ec2types.Volume
		wantRec  Recommendation
		wantSave float64
	}{
		{
			name:     "small gp2 migrates cleanly",
			v:        vol("vol-1", "gp2", "us-east-1a", 100, "i-1"),
			wantRec:  RecMigrate,
			wantSave: 100 * (gp2GB - gp3GB),
		},
		{
			name:     "large gp2 flagged for IOPS review",
			v:        vol("vol-2", "gp2", "us-east-1a", 2000, ""),
			wantRec:  RecReviewIOPS,
			wantSave: 2000 * (gp2GB - gp3GB),
		},
		{
			name:     "gp3 is skipped",
			v:        vol("vol-3", "gp3", "us-east-1a", 500, "i-3"),
			wantRec:  RecSkip,
			wantSave: 0,
		},
		{
			name:     "io2 is skipped",
			v:        vol("vol-4", "io2", "us-east-1a", 400, "i-4"),
			wantRec:  RecSkip,
			wantSave: 0,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := classify(tt.v, gp2GB, gp3GB)
			if got.Recommendation != tt.wantRec {
				t.Fatalf("recommendation = %s, want %s", got.Recommendation, tt.wantRec)
			}
			if got.MonthlySavings != tt.wantSave {
				t.Fatalf("savings = %.4f, want %.4f", got.MonthlySavings, tt.wantSave)
			}
		})
	}
}

func TestGP2IOPSThreshold(t *testing.T) {
	// 1000 GiB * 3 IOPS/GiB = 3000 == free tier → still MIGRATE (not over).
	if got := gp2IOPS(1000); got != 3000 {
		t.Fatalf("gp2IOPS(1000) = %d, want 3000", got)
	}
	// 1001 GiB → 3003 IOPS, just over the free tier → REVIEW_IOPS.
	gp2GB, gp3GB := Prices("us-east-1")
	if rec := classify(vol("v", "gp2", "z", 1001, ""), gp2GB, gp3GB).Recommendation; rec != RecReviewIOPS {
		t.Fatalf("1001 GiB rec = %s, want REVIEW_IOPS", rec)
	}
	if rec := classify(vol("v", "gp2", "z", 1000, ""), gp2GB, gp3GB).Recommendation; rec != RecMigrate {
		t.Fatalf("1000 GiB rec = %s, want MIGRATE", rec)
	}
	// tiny volume still gets the 100 IOPS floor.
	if got := gp2IOPS(1); got != 100 {
		t.Fatalf("gp2IOPS(1) = %d, want 100 floor", got)
	}
	// huge volume capped at 16000.
	if got := gp2IOPS(999999); got != 16000 {
		t.Fatalf("gp2IOPS(large) = %d, want 16000 cap", got)
	}
}

func TestScanTotalsAndOrderingAndPagination(t *testing.T) {
	m := &mockEC2{vols: []ec2types.Volume{
		vol("vol-gp3", "gp3", "us-east-1a", 500, "i-a"),   // skip
		vol("vol-small", "gp2", "us-east-1a", 100, "i-b"), // migrate, small savings
		vol("vol-big", "gp2", "us-east-1a", 2000, ""),     // review-iops, big savings
	}}
	rep, err := Scan(context.Background(), m, "us-east-1")
	if err != nil {
		t.Fatal(err)
	}
	if len(rep.Volumes) != 3 {
		t.Fatalf("pagination lost volumes: got %d, want 3", len(rep.Volumes))
	}
	if rep.GP2Count != 2 {
		t.Fatalf("gp2 count = %d, want 2", rep.GP2Count)
	}
	// REVIEW_IOPS volumes are not free savings: matching their gp2 IOPS on gp3
	// costs extra, so they must stay out of the conservative total and the
	// migrate count, and be reported separately instead.
	if rep.MigrateCount != 1 {
		t.Fatalf("migrate count = %d, want 1 (REVIEW_IOPS must not count)", rep.MigrateCount)
	}
	gp2GB, gp3GB := Prices("us-east-1")
	want := 100 * (gp2GB - gp3GB)
	if diff := rep.TotalSavings - want; diff > 1e-9 || diff < -1e-9 {
		t.Fatalf("total = %.6f, want %.6f (MIGRATE only)", rep.TotalSavings, want)
	}
	if rep.ReviewIOPSCount != 1 {
		t.Fatalf("review count = %d, want 1", rep.ReviewIOPSCount)
	}
	wantReview := 2000 * (gp2GB - gp3GB)
	if diff := rep.ReviewIOPSStorageDelta - wantReview; diff > 1e-9 || diff < -1e-9 {
		t.Fatalf("review storage delta = %.6f, want %.6f", rep.ReviewIOPSStorageDelta, wantReview)
	}
	// Biggest savings first.
	if rep.Volumes[0].VolumeID != "vol-big" {
		t.Fatalf("not savings-ordered: first = %s", rep.Volumes[0].VolumeID)
	}
	// gp3 (zero savings) sorts last.
	if rep.Volumes[2].VolumeID != "vol-gp3" {
		t.Fatalf("skip volume should sort last: %s", rep.Volumes[2].VolumeID)
	}
}

func TestScanReviewOnlyHasNoFreeSavings(t *testing.T) {
	m := &mockEC2{vols: []ec2types.Volume{vol("vol-big", "gp2", "us-east-1a", 2000, "")}}
	rep, err := Scan(context.Background(), m, "us-east-1")
	if err != nil {
		t.Fatal(err)
	}
	if rep.MigrateCount != 0 || rep.TotalSavings != 0 {
		t.Fatalf("review-only scan counted as free savings: count=%d total=%.2f",
			rep.MigrateCount, rep.TotalSavings)
	}
	var buf bytes.Buffer
	if err := rep.WriteTable(&buf); err != nil {
		t.Fatal(err)
	}
	if out := buf.String(); !strings.Contains(out, "1 need IOPS review") {
		t.Fatalf("table summary does not mention REVIEW_IOPS volumes:\n%s", out)
	}
}

func TestPricingFallback(t *testing.T) {
	// Known region uses its table entry.
	if gp2, gp3 := Prices("eu-central-1"); gp2 != 0.119 || gp3 != 0.0952 {
		t.Fatalf("eu-central-1 prices = %.4f/%.4f", gp2, gp3)
	}
	// Unknown region falls back to the reference.
	if gp2, gp3 := Prices("mars-north-1"); gp2 != defaultGP2PerGB || gp3 != defaultGP3PerGB {
		t.Fatalf("fallback prices = %.4f/%.4f", gp2, gp3)
	}
}

func TestReportJSONRoundTrip(t *testing.T) {
	m := &mockEC2{vols: []ec2types.Volume{vol("vol-1", "gp2", "us-east-1a", 200, "i-1")}}
	rep, err := Scan(context.Background(), m, "us-east-1")
	if err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	if err := rep.WriteJSON(&buf); err != nil {
		t.Fatal(err)
	}
	var back Report
	if err := json.Unmarshal(buf.Bytes(), &back); err != nil {
		t.Fatalf("json invalid: %v", err)
	}
	if back.MigrateCount != 1 || back.Region != "us-east-1" {
		t.Fatalf("round trip lost data: %+v", back)
	}
}

func TestReportTable(t *testing.T) {
	m := &mockEC2{vols: []ec2types.Volume{vol("vol-1", "gp2", "us-east-1a", 100, "i-1")}}
	rep, _ := Scan(context.Background(), m, "us-east-1")
	var buf bytes.Buffer
	if err := rep.WriteTable(&buf); err != nil {
		t.Fatal(err)
	}
	out := buf.String()
	if !strings.Contains(out, "MIGRATE") || !strings.Contains(out, "1 worth migrating") {
		t.Fatalf("table missing expected content:\n%s", out)
	}
}
