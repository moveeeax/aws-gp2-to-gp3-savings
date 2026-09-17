// Package finder enumerates EBS volumes and estimates the monthly dollar
// savings of migrating legacy gp2 volumes to gp3.
//
// gp3 is priced ~20% lower per provisioned GB than gp2 and, unlike gp2, does
// not couple IOPS to size: every gp3 volume includes 3,000 IOPS and 125 MB/s
// of baseline throughput for free. Any gp2 volume whose size stays under the
// point where gp2's size-derived IOPS exceed the gp3 baseline migrates with no
// performance loss and an immediate storage-price cut.
package finder

import (
	"context"
	"fmt"
	"sort"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	ec2types "github.com/aws/aws-sdk-go-v2/service/ec2/types"
)

// Recommendation classifies what should happen to a volume.
type Recommendation string

const (
	// RecMigrate means the volume is gp2 and moving it to gp3 is a clear win.
	RecMigrate Recommendation = "MIGRATE"
	// RecReviewIOPS means the volume is gp2 but its size gives it more baseline
	// IOPS than gp3's free tier (3,000); migrating still cuts the GB price but
	// you may want to provision extra gp3 IOPS to match — flagged, not counted
	// as free savings.
	RecReviewIOPS Recommendation = "REVIEW_IOPS"
	// RecSkip means the volume is not gp2 (already gp3/io1/io2/st1/sc1/standard).
	RecSkip Recommendation = "SKIP"
)

// Pricing constants. Per-GB-month USD in the reference region (us-east-1).
// gp2 and gp3 storage prices are stable and public; IOPS/throughput above the
// gp3 free tier are extra but not needed by any volume this tool recommends
// migrating for free.
const (
	defaultGP2PerGB = 0.10 // gp2: $0.10 /GB-month
	defaultGP3PerGB = 0.08 // gp3: $0.08 /GB-month (~20% cheaper)
	gp3FreeIOPS     = 3000 // IOPS included with every gp3 volume
	gp2IOPSPerGB    = 3.0  // gp2 grants 3 IOPS per provisioned GB
	gp2BaselineIOPS = 100  // gp2 volumes get at least 100 IOPS regardless of size
)

// regionPrices overrides the reference per-GB prices for regions that differ.
// Missing regions fall back to the us-east-1 reference; the ~20% gp2→gp3 delta
// holds across commercial regions even where the absolute price is higher.
var regionPrices = map[string][2]float64{
	// region: {gp2PerGB, gp3PerGB}
	"us-east-1":      {0.10, 0.08},
	"us-west-2":      {0.10, 0.08},
	"eu-central-1":   {0.119, 0.0952},
	"eu-west-1":      {0.11, 0.088},
	"ap-southeast-1": {0.12, 0.096},
	"ap-northeast-1": {0.12, 0.096},
}

// Prices returns the (gp2, gp3) per-GB-month USD prices for a region.
func Prices(region string) (gp2, gp3 float64) {
	if p, ok := regionPrices[region]; ok {
		return p[0], p[1]
	}
	return defaultGP2PerGB, defaultGP3PerGB
}

// gp2IOPS returns the baseline IOPS a gp2 volume of the given size receives.
// AWS caps gp2 baseline IOPS at 16,000.
func gp2IOPS(sizeGiB int32) int32 {
	iops := int32(float64(sizeGiB) * gp2IOPSPerGB)
	if iops < gp2BaselineIOPS {
		iops = gp2BaselineIOPS
	}
	if iops > 16000 {
		iops = 16000
	}
	return iops
}

// Volume is the finder's normalized, priced view of one EBS volume.
type Volume struct {
	VolumeID       string         `json:"volume_id"`
	AZ             string         `json:"availability_zone"`
	Type           string         `json:"volume_type"`
	SizeGiB        int32          `json:"size_gib"`
	GP2IOPS        int32          `json:"gp2_baseline_iops,omitempty"`
	State          string         `json:"state"`
	AttachedTo     string         `json:"attached_to,omitempty"`
	MonthlyGP2     float64        `json:"monthly_gp2_usd"`
	MonthlyGP3     float64        `json:"monthly_gp3_usd"`
	MonthlySavings float64        `json:"monthly_savings_usd"`
	Recommendation Recommendation `json:"recommendation"`
}

// migratable reports whether a volume's savings should be counted in the total.
func (v Volume) migratable() bool {
	return v.Recommendation == RecMigrate || v.Recommendation == RecReviewIOPS
}

// EC2API is the slice of the EC2 client the finder needs. Injecting it keeps
// the classifier unit-testable with no live AWS calls.
type EC2API interface {
	DescribeVolumes(ctx context.Context, in *ec2.DescribeVolumesInput, optFns ...func(*ec2.Options)) (*ec2.DescribeVolumesOutput, error)
}

// Report is the full scan result for a region.
type Report struct {
	Region       string   `json:"region"`
	Volumes      []Volume `json:"volumes"`
	GP2Count     int      `json:"gp2_count"`
	MigrateCount int      `json:"migrate_count"`
	TotalSavings float64  `json:"total_monthly_savings_usd"`
	GP2PerGBUSD  float64  `json:"gp2_per_gb_usd"`
	GP3PerGBUSD  float64  `json:"gp3_per_gb_usd"`
}

// Scan enumerates every EBS volume in the region, classifies each, and totals
// the monthly savings of migrating the gp2 volumes to gp3.
func Scan(ctx context.Context, client EC2API, region string) (*Report, error) {
	gp2GB, gp3GB := Prices(region)
	rep := &Report{Region: region, GP2PerGBUSD: gp2GB, GP3PerGBUSD: gp3GB}

	var token *string
	for {
		in := &ec2.DescribeVolumesInput{}
		if token != nil {
			in.NextToken = token
		}
		out, err := client.DescribeVolumes(ctx, in)
		if err != nil {
			return nil, fmt.Errorf("describe volumes: %w", err)
		}
		for _, v := range out.Volumes {
			rep.Volumes = append(rep.Volumes, classify(v, gp2GB, gp3GB))
		}
		if out.NextToken == nil || aws.ToString(out.NextToken) == "" {
			break
		}
		token = out.NextToken
	}

	for _, v := range rep.Volumes {
		if v.Type == string(ec2types.VolumeTypeGp2) {
			rep.GP2Count++
		}
		if v.migratable() {
			rep.MigrateCount++
			rep.TotalSavings += v.MonthlySavings
		}
	}

	// Deterministic ordering: biggest savings first, then by volume ID.
	sort.SliceStable(rep.Volumes, func(i, j int) bool {
		vi, vj := rep.Volumes[i], rep.Volumes[j]
		if vi.MonthlySavings != vj.MonthlySavings {
			return vi.MonthlySavings > vj.MonthlySavings
		}
		return vi.VolumeID < vj.VolumeID
	})
	return rep, nil
}

// classify turns a raw EC2 volume into a normalized, priced Volume.
func classify(v ec2types.Volume, gp2GB, gp3GB float64) Volume {
	vol := Volume{
		VolumeID: aws.ToString(v.VolumeId),
		AZ:       aws.ToString(v.AvailabilityZone),
		Type:     string(v.VolumeType),
		SizeGiB:  aws.ToInt32(v.Size),
		State:    string(v.State),
	}
	for _, a := range v.Attachments {
		if id := aws.ToString(a.InstanceId); id != "" {
			vol.AttachedTo = id
			break
		}
	}

	if v.VolumeType != ec2types.VolumeTypeGp2 {
		vol.Recommendation = RecSkip
		return vol
	}

	vol.GP2IOPS = gp2IOPS(vol.SizeGiB)
	vol.MonthlyGP2 = float64(vol.SizeGiB) * gp2GB
	vol.MonthlyGP3 = float64(vol.SizeGiB) * gp3GB
	// Compute savings from the per-GB delta rather than subtracting the two
	// rounded totals, so callers reproducing the number the obvious way
	// (size × (gp2−gp3)) get bit-identical results.
	vol.MonthlySavings = float64(vol.SizeGiB) * (gp2GB - gp3GB)

	if vol.GP2IOPS > gp3FreeIOPS {
		// gp2 already derives more IOPS from its size than gp3 hands out free.
		// Storage still gets cheaper, but flag it so the operator can decide
		// whether to provision matching gp3 IOPS.
		vol.Recommendation = RecReviewIOPS
	} else {
		vol.Recommendation = RecMigrate
	}
	return vol
}
