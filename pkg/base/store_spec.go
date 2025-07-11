// Copyright 2016 The Cockroach Authors.
//
// Use of this software is governed by the CockroachDB Software License
// included in the /LICENSE file.

package base

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"unicode"

	"github.com/cockroachdb/cockroach/pkg/cli/cliflags"
	"github.com/cockroachdb/cockroach/pkg/storage/storageconfig"
	"github.com/cockroachdb/cockroach/pkg/util/humanizeutil"
	"github.com/cockroachdb/errors"
	"github.com/cockroachdb/errors/oserror"
	"github.com/cockroachdb/pebble"
	"github.com/cockroachdb/redact"
	humanize "github.com/dustin/go-humanize"
	"github.com/spf13/pflag"
)

// This file implements method receivers for server.Config struct
// -- 'Stores', which satisfies pflag's value interface

// MinimumStoreSize is the smallest size in bytes that a store can have. This
// number is based on config's defaultZoneConfig's RangeMaxBytes, which is
// extremely stable. To avoid adding the dependency on config here, it is just
// hard coded to 640MiB.
const MinimumStoreSize = 10 * 64 << 20

// GetAbsoluteFSPath takes a (possibly relative) and returns the absolute path.
// Returns an error if the path begins with '~' or Abs fails.
// 'fieldName' is used in error strings.
func GetAbsoluteFSPath(fieldName string, p string) (string, error) {
	if p[0] == '~' {
		return "", fmt.Errorf("%s cannot start with '~': %s", fieldName, p)
	}

	ret, err := filepath.Abs(p)
	if err != nil {
		return "", errors.Wrapf(err, "could not find absolute path for %s %s", fieldName, p)
	}
	return ret, nil
}

func parseStoreProvisionedRate(
	field redact.SafeString, value string,
) (storageconfig.ProvisionedRate, error) {
	split := strings.Split(value, "=")
	if len(split) != 2 {
		return storageconfig.ProvisionedRate{}, errors.Errorf("%s field has invalid value %s", field, value)
	}
	subField := split[0]
	subValue := split[1]
	if subField != "bandwidth" {
		return storageconfig.ProvisionedRate{}, errors.Errorf("%s field does not have bandwidth sub-field", field)
	}
	if len(subValue) == 0 {
		return storageconfig.ProvisionedRate{}, errors.Errorf("%s field has no value specified for bandwidth", field)
	}
	if len(subValue) <= 2 || subValue[len(subValue)-2:] != "/s" {
		return storageconfig.ProvisionedRate{},
			errors.Errorf("%s field does not have bandwidth sub-field %s ending in /s",
				field, subValue)
	}
	bandwidthString := subValue[:len(subValue)-2]
	bandwidth, err := humanizeutil.ParseBytes(bandwidthString)
	if err != nil {
		return storageconfig.ProvisionedRate{},
			errors.Wrapf(err, "could not parse bandwidth in field %s", field)
	}
	if bandwidth == 0 {
		return storageconfig.ProvisionedRate{},
			errors.Errorf("%s field is trying to set bandwidth to 0", field)
	}
	return storageconfig.ProvisionedRate{ProvisionedBandwidth: bandwidth}, nil
}

// StoreSpec contains the details that can be specified in the cli pertaining
// to the --store flag.
type StoreSpec = storageconfig.Store

// StoreSpecCmdLineString returns a fully parsable version of the store spec.
func StoreSpecCmdLineString(ss storageconfig.Store) string {
	// TODO(jackson): Implement redact.SafeFormatter
	var buffer bytes.Buffer
	if len(ss.Path) != 0 {
		fmt.Fprintf(&buffer, "path=%s,", ss.Path)
	}
	if ss.InMemory {
		fmt.Fprint(&buffer, "type=mem,")
	}
	if ss.Size.Bytes > 0 {
		fmt.Fprintf(&buffer, "size=%s,", humanizeutil.IBytes(ss.Size.Bytes))
	}
	if ss.Size.Percent > 0 {
		fmt.Fprintf(&buffer, "size=%s%%,", humanize.Ftoa(ss.Size.Percent))
	}
	if ss.BallastSize != nil {
		if ss.BallastSize.Bytes > 0 {
			fmt.Fprintf(&buffer, "ballast-size=%s,", humanizeutil.IBytes(ss.BallastSize.Bytes))
		}
		if ss.BallastSize.Percent > 0 {
			fmt.Fprintf(&buffer, "ballast-size=%s%%,", humanize.Ftoa(ss.BallastSize.Percent))
		}
	}
	if len(ss.Attributes) > 0 {
		fmt.Fprint(&buffer, "attrs=")
		for i, attr := range ss.Attributes {
			if i != 0 {
				fmt.Fprint(&buffer, ":")
			}
			buffer.WriteString(attr)
		}
		fmt.Fprintf(&buffer, ",")
	}
	if len(ss.PebbleOptions) > 0 {
		optsStr := strings.Replace(ss.PebbleOptions, "\n", " ", -1)
		fmt.Fprint(&buffer, "pebble=")
		fmt.Fprint(&buffer, optsStr)
		fmt.Fprint(&buffer, ",")
	}
	if ss.ProvisionedRate.ProvisionedBandwidth > 0 {
		fmt.Fprintf(&buffer, "provisioned-rate=bandwidth=%s/s,",
			humanizeutil.IBytes(ss.ProvisionedRate.ProvisionedBandwidth))
	}
	// Trim the extra comma from the end if it exists.
	if l := buffer.Len(); l > 0 {
		buffer.Truncate(l - 1)
	}
	return buffer.String()
}

// NewStoreSpec parses the string passed into a --store flag and returns a
// StoreSpec if it is correctly parsed.
// There are five possible fields that can be passed in, comma separated:
//   - path=xxx The directory in which the rocks db instance should be
//     located, required unless using an in memory storage.
//   - type=mem This specifies that the store is an in memory storage instead of
//     an on disk one. mem is currently the only other type available.
//   - size=xxx The optional maximum size of the storage. This can be in one of a
//     few different formats.
//   - 10000000000     -> 10000000000 bytes
//   - 20GB            -> 20000000000 bytes
//   - 20GiB           -> 21474836480 bytes
//   - 0.02TiB         -> 21474836480 bytes
//   - 20%             -> 20% of the available space
//   - 0.2             -> 20% of the available space
//   - attrs=xxx:yyy:zzz A colon separated list of optional attributes.
//   - provisioned-rate=bandwidth=<bandwidth-bytes/s> The provisioned-rate can be
//     used for admission control for operations on the store and if unspecified,
//     a cluster setting (kvadmission.store.provisioned_bandwidth) will be used.
//
// Note that commas are forbidden within any field name or value.
func NewStoreSpec(value string) (StoreSpec, error) {
	const pathField = "path"
	if len(value) == 0 {
		return StoreSpec{}, fmt.Errorf("no value specified")
	}
	var ss StoreSpec
	used := make(map[string]struct{})
	for _, split := range strings.Split(value, ",") {
		if len(split) == 0 {
			continue
		}
		subSplits := strings.SplitN(split, "=", 2)
		var field string
		var value string
		if len(subSplits) == 1 {
			field = pathField
			value = subSplits[0]
		} else {
			field = strings.ToLower(subSplits[0])
			value = subSplits[1]
		}
		if _, ok := used[field]; ok {
			return StoreSpec{}, fmt.Errorf("%s field was used twice in store definition", field)
		}
		used[field] = struct{}{}

		if len(field) == 0 {
			continue
		}
		if len(value) == 0 {
			return StoreSpec{}, fmt.Errorf("no value specified for %s", field)
		}

		switch field {
		case pathField:
			ss.Path = value
		case "size":
			var err error
			constraints := storageconfig.SizeSpecConstraints{
				MinBytes:   MinimumStoreSize,
				MinPercent: 1,
				MaxPercent: 100,
			}
			ss.Size, err = storageconfig.ParseSizeSpec(value, constraints)
			if err != nil {
				return StoreSpec{}, err
			}
		case "ballast-size":
			constraints := storageconfig.SizeSpecConstraints{
				MaxPercent: 50,
			}
			ballastSize, err := storageconfig.ParseSizeSpec(value, constraints)
			if err != nil {
				return StoreSpec{}, errors.Wrap(err, "ballast")
			}
			ss.BallastSize = &ballastSize
		case "attrs":
			// Check to make sure there are no duplicate attributes.
			attrMap := make(map[string]struct{})
			for _, attribute := range strings.Split(value, ":") {
				if _, ok := attrMap[attribute]; ok {
					return StoreSpec{}, fmt.Errorf("duplicate attribute given for store: %s", attribute)
				}
				attrMap[attribute] = struct{}{}
			}
			for attribute := range attrMap {
				ss.Attributes = append(ss.Attributes, attribute)
			}
			sort.Strings(ss.Attributes)
		case "type":
			if value == "mem" {
				ss.InMemory = true
			} else {
				return StoreSpec{}, fmt.Errorf("%s is not a valid store type", value)
			}
		case "pebble":
			// Pebble options are supplied in the Pebble OPTIONS ini-like
			// format, but allowing any whitespace to delimit lines. Convert
			// the options to a newline-delimited format. This isn't a trivial
			// character replacement because whitespace may appear within a
			// stanza, eg ["Level 0"].
			value = strings.TrimSpace(value)
			var buf bytes.Buffer
			for len(value) > 0 {
				i := strings.IndexFunc(value, func(r rune) bool {
					return r == '[' || unicode.IsSpace(r)
				})
				switch {
				case i == -1:
					buf.WriteString(value)
					value = value[len(value):]
				case value[i] == '[':
					// If there's whitespace within [ ], we write it verbatim.
					j := i + strings.IndexRune(value[i:], ']')
					buf.WriteString(value[:j+1])
					value = value[j+1:]
				case unicode.IsSpace(rune(value[i])):
					// NB: This doesn't handle multibyte whitespace.
					buf.WriteString(value[:i])
					buf.WriteRune('\n')
					value = strings.TrimSpace(value[i+1:])
				}
			}

			// Parse the options just to fail early if invalid. We'll parse
			// them again later when constructing the store engine.
			var opts pebble.Options
			if err := opts.Parse(buf.String(), nil); err != nil {
				return StoreSpec{}, err
			}
			ss.PebbleOptions = buf.String()
		case "provisioned-rate":
			rateSpec, err := parseStoreProvisionedRate("provisioned-rate", value)
			if err != nil {
				return StoreSpec{}, err
			}
			ss.ProvisionedRate = rateSpec

		default:
			return StoreSpec{}, fmt.Errorf("%s is not a valid store field", field)
		}
	}
	if ss.InMemory {
		// Only in memory stores don't need a path and require a size.
		if ss.Path != "" {
			return StoreSpec{}, fmt.Errorf("path specified for in memory store")
		}
		if ss.Size.Percent == 0 && ss.Size.Bytes == 0 {
			return StoreSpec{}, fmt.Errorf("size must be specified for an in memory store")
		}
		if ss.BallastSize != nil {
			return StoreSpec{}, fmt.Errorf("ballast-size specified for in memory store")
		}
	} else if ss.Path == "" {
		return StoreSpec{}, fmt.Errorf("no path specified")
	}
	return ss, nil
}

// StoreSpecList contains a slice of StoreSpecs that implements pflag's value
// interface.
type StoreSpecList struct {
	Specs   []StoreSpec
	updated bool // updated is used to determine if specs only contain the default value.
}

var _ pflag.Value = &StoreSpecList{}

// String returns a string representation of all the StoreSpecs. This is part
// of pflag's value interface.
func (ssl StoreSpecList) String() string {
	var buffer bytes.Buffer
	for _, ss := range ssl.Specs {
		fmt.Fprintf(&buffer, "--%s=%s ", cliflags.Store.Name, StoreSpecCmdLineString(ss))
	}
	// Trim the extra space from the end if it exists.
	if l := buffer.Len(); l > 0 {
		buffer.Truncate(l - 1)
	}
	return buffer.String()
}

// AuxiliaryDir is the path of the auxiliary dir relative to an engine.Engine's
// root directory. It must not be changed without a proper migration.
const AuxiliaryDir = "auxiliary"

// EmergencyBallastFile returns the path (relative to a data directory) used
// for an emergency ballast file. The returned path must be stable across
// releases (eg, we cannot change these constants), otherwise we may duplicate
// ballasts.
func EmergencyBallastFile(pathJoin func(...string) string, dataDir string) string {
	return pathJoin(dataDir, AuxiliaryDir, "EMERGENCY_BALLAST")
}

// PreventedStartupFile is the filename (relative to 'dir') used for files that
// can block server startup.
func PreventedStartupFile(dir string) string {
	return filepath.Join(dir, "_CRITICAL_ALERT.txt")
}

// PriorCriticalAlertError attempts to read the
// PreventedStartupFile for each store directory and returns their
// contents as a structured error.
//
// These files typically request operator intervention after a
// corruption event by preventing the affected node(s) from starting
// back up.
func (ssl StoreSpecList) PriorCriticalAlertError() (err error) {
	addError := func(newErr error) {
		if err == nil {
			err = errors.New("startup forbidden by prior critical alert")
		}
		// We use WithDetailf here instead of errors.CombineErrors
		// because we want the details to be printed to the screen
		// (combined errors only show up via %+v).
		err = errors.WithDetailf(err, "%v", newErr)
	}
	for _, ss := range ssl.Specs {
		if ss.InMemory {
			continue
		}
		path := PreventedStartupFile(filepath.Join(ss.Path, AuxiliaryDir))
		if path == "" {
			continue
		}
		b, err := os.ReadFile(path)
		if err != nil {
			if !oserror.IsNotExist(err) {
				addError(errors.Wrapf(err, "%s", path))
			}
			continue
		}
		addError(errors.Newf("From %s:\n\n%s\n", path, b))
	}
	return err
}

// Type returns the underlying type in string form. This is part of pflag's
// value interface.
func (ssl *StoreSpecList) Type() string {
	return "StoreSpec"
}

// Set adds a new value to the StoreSpecValue. It is the important part of
// pflag's value interface.
func (ssl *StoreSpecList) Set(value string) error {
	spec, err := NewStoreSpec(value)
	if err != nil {
		return err
	}
	if !ssl.updated {
		ssl.Specs = []StoreSpec{spec}
		ssl.updated = true
	} else {
		ssl.Specs = append(ssl.Specs, spec)
	}
	return nil
}

// PopulateWithEncryptionOpts iterates through the EncryptionSpecList and looks
// for matching paths in the StoreSpecList and WAL failover config. Any
// unmatched EncryptionSpec causes an error.
func PopulateWithEncryptionOpts(
	storeSpecs StoreSpecList,
	walFailoverConfig *storageconfig.WALFailover,
	encryptionSpecs storageconfig.EncryptionSpecList,
) error {
	for _, es := range encryptionSpecs.Specs {
		var storeMatched bool
		for i := range storeSpecs.Specs {
			if !es.PathMatches(storeSpecs.Specs[i].Path) {
				continue
			}

			// Found a matching path.
			if storeSpecs.Specs[i].EncryptionOptions != nil {
				return fmt.Errorf("store with path %s already has an encryption setting",
					storeSpecs.Specs[i].Path)
			}

			storeSpecs.Specs[i].EncryptionOptions = &es.Options
			storeMatched = true
			break
		}
		pathMatched, err := maybeSetExternalPathEncryption(&walFailoverConfig.Path, es)
		if err != nil {
			return err
		}
		prevPathMatched, err := maybeSetExternalPathEncryption(&walFailoverConfig.PrevPath, es)
		if err != nil {
			return err
		}
		if !storeMatched && !pathMatched && !prevPathMatched {
			return fmt.Errorf("no usage of path %s found for encryption setting: %v", es.Path, es)
		}
	}
	return nil
}

// maybeSetExternalPathEncryption updates an ExternalPath to contain the provided
// encryption options if the path matches. The ExternalPath must not already have
// an encryption setting.
func maybeSetExternalPathEncryption(
	externalPath *storageconfig.ExternalPath, es storageconfig.StoreEncryptionSpec,
) (found bool, err error) {
	if !externalPath.IsSet() || !es.PathMatches(externalPath.Path) {
		return false, nil
	}
	// NB: The external paths WALFailoverConfig.Path and
	// WALFailoverConfig.PrevPath are only ever set in single-store
	// configurations. In multi-store with among-stores failover mode, these
	// will be empty (so we won't encounter the same path twice).
	if externalPath.Encryption != nil {
		return false, fmt.Errorf("WAL failover path %s already has an encryption setting",
			externalPath.Path)
	}
	externalPath.Encryption = &es.Options
	return true, nil
}
