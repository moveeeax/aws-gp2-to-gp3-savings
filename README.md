# aws-gp2-to-gp3-savings

[![ci](https://github.com/moveeeax/aws-gp2-to-gp3-savings/actions/workflows/ci.yml/badge.svg)](https://github.com/moveeeax/aws-gp2-to-gp3-savings/actions/workflows/ci.yml)
[![Go](https://img.shields.io/badge/go-1.24%2B-00ADD8?logo=go&logoColor=white)](https://go.dev)
[![License: MIT](https://img.shields.io/badge/license-MIT-blue.svg)](LICENSE)

Scan EBS volumes for legacy **gp2** and total the monthly dollar savings of migrating them to **gp3**.

gp3 costs ~20% less per provisioned GB than gp2 ($0.08 vs $0.10 /GB-month in us-east-1) and, unlike gp2, decouples IOPS from size: every gp3 volume ships with **3,000 IOPS and 125 MB/s** of baseline throughput for free. The migration is a live modify with no downtime. On IOPS, gp3's free 3,000 covers every gp2 volume up to 1,000 GiB (including gp2 burst). Throughput is the one thing to check: gp2 volumes larger than 170 GiB can reach 250 MiB/s (sustained from 334 GiB), above gp3's free 125 MiB/s, and this tool does not look at throughput. The savings are boring per volume and significant per account.

## How it works

1. `DescribeVolumes` (paginated) lists every EBS volume in the region.
2. Each volume is classified:
   - **`MIGRATE`** — gp2 whose size-derived IOPS stay at or below gp3's free 3,000 → clean win, savings counted.
   - **`REVIEW_IOPS`** — gp2 larger than 1,000 GiB, where gp2's `3 IOPS/GiB` already exceeds gp3's free tier. Storage still gets cheaper, but matching the gp2 IOPS on gp3 is billed extra, so the per-volume storage delta is **not** added to the total. It is reported separately (`review_iops_count`, `review_iops_storage_delta_usd`) as an upper bound — flagged so it's a conscious call.
   - **`SKIP`** — not gp2 (gp3/io1/io2/st1/sc1/standard).
3. gp2 and gp3 are priced from a static per-region table (`size × $/GB-month`); the per-volume delta of `MIGRATE` volumes is summed.
4. It prints a table (or `--json`) and **exits `1` when `MIGRATE` volumes are found**, so it doubles as a CI/cost gate. `REVIEW_IOPS` volumes alone do not fail the gate.

Tests run entirely offline against an injected client interface — no live AWS calls.

## Install

```sh
go install github.com/moveeeax/aws-gp2-to-gp3-savings/cmd/aws-gp2-to-gp3-savings@latest
```

Or build from source:

```sh
git clone https://github.com/moveeeax/aws-gp2-to-gp3-savings.git
cd aws-gp2-to-gp3-savings
go build ./cmd/aws-gp2-to-gp3-savings
```

## Usage

```
usage: aws-gp2-to-gp3-savings [--region R] [--json] [--min-savings N]

  --region string       AWS region to scan (default: from env/profile)
  --json                emit JSON instead of a table
  --min-savings float   only fail (exit 1) when total monthly savings exceeds this USD amount
```

Credentials come from the standard AWS chain (env vars, shared config, SSO, instance role). The read-only `ec2:DescribeVolumes` permission is enough.

### Table output

```sh
$ aws-gp2-to-gp3-savings --region us-east-1
RECOMMENDATION  VOLUME_ID                TYPE  SIZE_GIB  ATTACHED             SAVINGS_$/MO
REVIEW_IOPS     vol-0a1b2c3d4e5f60011    gp2   2000      -                    40.00
MIGRATE         vol-0a1b2c3d4e5f60022    gp2   500       i-0abcd1234ef567890  10.00
SKIP            vol-0a1b2c3d4e5f60033    gp3   300       i-0999aaaa8888bbbb7  0.00

3 volumes, 2 gp2, 1 worth migrating — estimated savings $10.00/mo (region us-east-1 @ gp2 $0.1000 vs gp3 $0.0800 /GB)
1 need IOPS review — storage delta up to $40.00/mo before extra gp3 IOPS, not included above
```

### JSON output

```sh
$ aws-gp2-to-gp3-savings --region us-east-1 --json
```

See [`examples/sample-report.json`](examples/sample-report.json) for the shape.

## Pricing notes

Prices are storage-only ($/GB-month) from a small static table, defaulting to the us-east-1 reference where a region isn't listed (no warning is printed, so check `gp2_per_gb_usd`/`gp3_per_gb_usd` in the output for other regions). The ~20% gp2→gp3 delta holds across commercial regions even where the absolute price is higher. The total covers only `MIGRATE` volumes, whose gp2 baseline IOPS fit in gp3's free 3,000, so on IOPS it is a conservative floor. `REVIEW_IOPS` volumes are left out of it because matching their IOPS on gp3 costs extra. Throughput is not modeled: if a `MIGRATE` volume over 170 GiB actually needs more than 125 MiB/s, provisioning extra gp3 throughput reduces its saving.

## Exit codes

| Code | Meaning |
|------|---------|
| `0`  | No `MIGRATE` volumes (or total savings ≤ `--min-savings`); `REVIEW_IOPS` alone exits `0` |
| `1`  | `MIGRATE` volumes found — CI gate |
| `2`  | Runtime error (credentials, region, AWS API) |

## License

MIT — see [LICENSE](LICENSE).

---

**Archived on 2026-11-05.** AWS Compute Optimizer gives free gp2-to-gp3 recommendations with savings, using real IOPS/throughput metrics this tool does not model. The only angles left are unattached volumes (Compute Optimizer skips them) and a no-opt-in, single-call CI gate.
