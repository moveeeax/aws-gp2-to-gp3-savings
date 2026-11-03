// Command aws-gp2-to-gp3-savings scans a region for legacy gp2 EBS volumes and
// totals the monthly dollar savings of migrating them to gp3.
//
// Exit codes:
//
//	0  no gp2 volumes worth migrating
//	1  MIGRATE gp2 volumes found (CI gate); REVIEW_IOPS alone does not fail
//	2  runtime error (AWS/credentials/flags)
package main

import (
	"context"
	"flag"
	"fmt"
	"os"

	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/ec2"

	"github.com/moveeeax/aws-gp2-to-gp3-savings/internal/finder"
)

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

func run(args []string, stdout, stderr *os.File) int {
	fs := flag.NewFlagSet("aws-gp2-to-gp3-savings", flag.ContinueOnError)
	fs.SetOutput(stderr)
	region := fs.String("region", "", "AWS region to scan (default: from env/profile)")
	asJSON := fs.Bool("json", false, "emit JSON instead of a table")
	minSavings := fs.Float64("min-savings", 0, "only fail (exit 1) when total monthly savings exceeds this USD amount")
	fs.Usage = func() {
		fmt.Fprintf(stderr, "usage: aws-gp2-to-gp3-savings [--region R] [--json] [--min-savings N]\n\n")
		fmt.Fprintf(stderr, "Scans EBS volumes and reports the monthly savings of migrating gp2 to gp3.\n\n")
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		return 2
	}

	ctx := context.Background()
	var opts []func(*config.LoadOptions) error
	if *region != "" {
		opts = append(opts, config.WithRegion(*region))
	}
	cfg, err := config.LoadDefaultConfig(ctx, opts...)
	if err != nil {
		fmt.Fprintf(stderr, "error: load AWS config: %v\n", err)
		return 2
	}
	if cfg.Region == "" {
		fmt.Fprintf(stderr, "error: no region set (use --region or AWS_REGION)\n")
		return 2
	}

	client := ec2.NewFromConfig(cfg)
	rep, err := finder.Scan(ctx, client, cfg.Region)
	if err != nil {
		fmt.Fprintf(stderr, "error: %v\n", err)
		return 2
	}

	if *asJSON {
		if err := rep.WriteJSON(stdout); err != nil {
			fmt.Fprintf(stderr, "error: %v\n", err)
			return 2
		}
	} else {
		if err := rep.WriteTable(stdout); err != nil {
			fmt.Fprintf(stderr, "error: %v\n", err)
			return 2
		}
	}

	if rep.MigrateCount > 0 && rep.TotalSavings > *minSavings {
		return 1
	}
	return 0
}
