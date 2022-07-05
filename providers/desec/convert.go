package desec

// Convert the provider's native record description to models.RecordConfig.

import (
	"fmt"

	"github.com/StackExchange/dnscontrol/v3/models"
	"github.com/StackExchange/dnscontrol/v3/pkg/printer"
)

// nativeToRecord takes a DNS record from deSEC and returns a native RecordConfig struct.
func nativeToRecords(n resourceRecord, origin string) (rcs []*models.RecordConfig, err error) {

	// deSEC returns all the values for a given label/rtype pair in each
	// resourceRecord.  In other words, if there are multiple A
	// records for a label, all the IP addresses are listed in
	// n.Records rather than having many resourceRecord's.
	// We must split them out into individual records, one for each value.
	for _, value := range n.Records {
		rc := &models.RecordConfig{
			TTL:      n.TTL,
			Original: n,
		}
		rc.SetLabel(n.Subname, origin)

		var err error
		switch rtype := n.Type; rtype {
		case "TXT":
			err = rc.SetTargetTXTQuotedFields(value)
		default:
			err = rc.PopulateFromString(rtype, value, origin)
		}
		if err != nil {
			return nil, fmt.Errorf("unparsable record received from deSEC: %w", err)
		}

		rcs = append(rcs, rc)
	}

	return rcs, nil
}

func recordsToNative(rcs []*models.RecordConfig, origin string) []resourceRecord {
	// Take a list of RecordConfig and return an equivalent list of resourceRecord.
	// deSEC requires one resourceRecord for each label:key tuple, therefore we
	// might collapse many RecordConfig into one resourceRecord.

	var keys = map[models.RecordKey]*resourceRecord{}
	var zrs []resourceRecord
	for _, r := range rcs {
		label := r.GetLabel()
		if label == "@" {
			label = ""
		}
		key := r.Key()

		if zr, ok := keys[key]; !ok {
			// Allocate a new ZoneRecord:
			zr := resourceRecord{
				Type:    r.Type,
				TTL:     r.TTL,
				Subname: label,
				Records: []string{r.GetTargetCombined()},
			}
			keys[key] = &zr
		} else {
			zr.Records = append(zr.Records, r.GetTargetCombined())

			if r.TTL != zr.TTL {
				printer.Warnf("All TTLs for a rrset (%v) must be the same. Using smaller of %v and %v.\n", key, r.TTL, zr.TTL)
				if r.TTL < zr.TTL {
					zr.TTL = r.TTL
				}
			}

		}
	}

	for _, zr := range keys {
		zrs = append(zrs, *zr)
	}
	return zrs
}
