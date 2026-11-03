package finder

import (
	"encoding/json"
	"fmt"
	"io"
	"text/tabwriter"
)

// WriteJSON renders the report as indented JSON.
func (r *Report) WriteJSON(w io.Writer) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(r)
}

// WriteTable renders a human-readable table plus a summary line.
func (r *Report) WriteTable(w io.Writer) error {
	tw := tabwriter.NewWriter(w, 0, 2, 2, ' ', 0)
	fmt.Fprintln(tw, "RECOMMENDATION\tVOLUME_ID\tTYPE\tSIZE_GIB\tATTACHED\tSAVINGS_$/MO")
	for _, v := range r.Volumes {
		att := v.AttachedTo
		if att == "" {
			att = "-"
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%d\t%s\t%.2f\n",
			v.Recommendation, v.VolumeID, v.Type, v.SizeGiB, att, v.MonthlySavings)
	}
	if err := tw.Flush(); err != nil {
		return err
	}
	fmt.Fprintf(w, "\n%d volumes, %d gp2, %d worth migrating — estimated savings $%.2f/mo (region %s @ gp2 $%.4f vs gp3 $%.4f /GB)\n",
		len(r.Volumes), r.GP2Count, r.MigrateCount, r.TotalSavings, r.Region, r.GP2PerGBUSD, r.GP3PerGBUSD)
	if r.ReviewIOPSCount > 0 {
		fmt.Fprintf(w, "%d need IOPS review — storage delta up to $%.2f/mo before extra gp3 IOPS, not included above\n",
			r.ReviewIOPSCount, r.ReviewIOPSStorageDelta)
	}
	return nil
}
