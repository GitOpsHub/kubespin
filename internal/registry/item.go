package registry

import (
	"fmt"
	"strconv"
	"time"

	"github.com/aws/aws-sdk-go-v2/service/dynamodb/types"

	"github.com/GitOpsHub/kubespin/internal/core"
)

// Attribute names. Only ClusterID, Provider, and Phase are part of the table's
// key schema or its index; the rest are schemaless and defined only here.
const (
	attrClusterID      = "ClusterID"
	attrPhase          = "Phase"
	attrProvider       = "Provider"
	attrRegion         = "Region"
	attrAccess         = "Access"
	attrProfileName    = "ProfileName"
	attrProfileVersion = "ProfileVersion"
	attrOIDCIssuer     = "OIDCIssuer"
	attrVersion        = "Version"
	attrLastReportedAt = "LastReportedAt"
	attrFindings       = "Findings"
	attrFindingsAt     = "FindingsAt"
	attrCreatedAt      = "CreatedAt"
	attrUpdatedAt      = "UpdatedAt"
	attrLeaseHolder    = "LeaseHolder"
	attrLeaseExpiresAt = "LeaseExpiresAt"
)

// Lease expiry is stored as epoch milliseconds rather than a timestamp string,
// because the acquire condition compares it with `<` and numeric comparison is
// unambiguous. RFC3339Nano strings are variable-width and would not order
// correctly under a string comparison.
func epochMillis(t time.Time) string { return strconv.FormatInt(t.UTC().UnixMilli(), 10) }

func fromEpochMillis(s string) (time.Time, error) {
	ms, err := strconv.ParseInt(s, 10, 64)
	if err != nil {
		return time.Time{}, fmt.Errorf("parsing epoch milliseconds %q: %w", s, err)
	}
	return time.UnixMilli(ms).UTC(), nil
}

func marshalRecord(rec Record) map[string]types.AttributeValue {
	item := map[string]types.AttributeValue{
		attrClusterID:      &types.AttributeValueMemberS{Value: rec.ClusterID.String()},
		attrPhase:          &types.AttributeValueMemberS{Value: rec.Phase.String()},
		attrProvider:       &types.AttributeValueMemberS{Value: rec.Provider.String()},
		attrRegion:         &types.AttributeValueMemberS{Value: rec.Region},
		attrAccess:         &types.AttributeValueMemberS{Value: rec.Access.String()},
		attrProfileName:    &types.AttributeValueMemberS{Value: rec.Profile.Name},
		attrProfileVersion: &types.AttributeValueMemberS{Value: rec.Profile.Version},
		attrVersion:        &types.AttributeValueMemberN{Value: strconv.FormatInt(rec.Version, 10)},
		attrCreatedAt:      &types.AttributeValueMemberS{Value: rec.CreatedAt.UTC().Format(time.RFC3339Nano)},
		attrUpdatedAt:      &types.AttributeValueMemberS{Value: rec.UpdatedAt.UTC().Format(time.RFC3339Nano)},
	}

	// Absent rather than empty: "never reported" and "reported at the zero time"
	// must not be the same item.
	if !rec.LastReportedAt.IsZero() {
		item[attrLastReportedAt] = &types.AttributeValueMemberS{
			Value: rec.LastReportedAt.UTC().Format(time.RFC3339Nano),
		}
	}
	if rec.OIDCIssuer != "" {
		item[attrOIDCIssuer] = &types.AttributeValueMemberS{Value: rec.OIDCIssuer}
	}
	if rec.Lease != nil {
		item[attrLeaseHolder] = &types.AttributeValueMemberS{Value: rec.Lease.Holder}
		item[attrLeaseExpiresAt] = &types.AttributeValueMemberN{Value: epochMillis(rec.Lease.ExpiresAt)}
	}
	// Absent, like LastReportedAt, when the cluster has never been audited —
	// distinct from an empty Findings list, which means a clean audit ran.
	if !rec.FindingsAt.IsZero() {
		values := make([]types.AttributeValue, len(rec.Findings))
		for i, f := range rec.Findings {
			values[i] = &types.AttributeValueMemberS{Value: f}
		}
		item[attrFindings] = &types.AttributeValueMemberL{Value: values}
		item[attrFindingsAt] = &types.AttributeValueMemberS{Value: rec.FindingsAt.UTC().Format(time.RFC3339Nano)}
	}

	return item
}

func unmarshalRecord(item map[string]types.AttributeValue) (Record, error) {
	if len(item) == 0 {
		return Record{}, fmt.Errorf("%w: empty item", ErrNotFound)
	}

	rec := Record{
		ClusterID: core.ClusterID(stringAttr(item, attrClusterID)),
		Phase:     core.Phase(stringAttr(item, attrPhase)),
		Provider:  core.Provider(stringAttr(item, attrProvider)),
		Region:    stringAttr(item, attrRegion),
		Access:    core.Access(stringAttr(item, attrAccess)),
		Profile: core.ProfileRef{
			Name:    stringAttr(item, attrProfileName),
			Version: stringAttr(item, attrProfileVersion),
		},
		OIDCIssuer: stringAttr(item, attrOIDCIssuer),
	}

	version, err := numberAttr(item, attrVersion)
	if err != nil {
		return Record{}, err
	}
	rec.Version = version

	for _, field := range []struct {
		name string
		into *time.Time
	}{
		{attrCreatedAt, &rec.CreatedAt},
		{attrUpdatedAt, &rec.UpdatedAt},
		{attrLastReportedAt, &rec.LastReportedAt},
		{attrFindingsAt, &rec.FindingsAt},
	} {
		raw := stringAttr(item, field.name)
		if raw == "" {
			continue
		}
		parsed, err := time.Parse(time.RFC3339Nano, raw)
		if err != nil {
			return Record{}, fmt.Errorf("parsing %s %q: %w", field.name, raw, err)
		}
		*field.into = parsed.UTC()
	}

	if list, ok := item[attrFindings].(*types.AttributeValueMemberL); ok {
		findings := make([]string, len(list.Value))
		for i, v := range list.Value {
			s, ok := v.(*types.AttributeValueMemberS)
			if !ok {
				return Record{}, fmt.Errorf("record %s has a non-string finding at index %d", rec.ClusterID, i)
			}
			findings[i] = s.Value
		}
		rec.Findings = findings
	}

	if holder := stringAttr(item, attrLeaseHolder); holder != "" {
		expiresRaw, ok := item[attrLeaseExpiresAt].(*types.AttributeValueMemberN)
		if !ok {
			return Record{}, fmt.Errorf("record %s has a lease holder but no expiry", rec.ClusterID)
		}
		expires, err := fromEpochMillis(expiresRaw.Value)
		if err != nil {
			return Record{}, err
		}
		rec.Lease = &Lease{Holder: holder, ExpiresAt: expires}
	}

	return rec, nil
}

func stringAttr(item map[string]types.AttributeValue, name string) string {
	if v, ok := item[name].(*types.AttributeValueMemberS); ok {
		return v.Value
	}
	return ""
}

func numberAttr(item map[string]types.AttributeValue, name string) (int64, error) {
	v, ok := item[name].(*types.AttributeValueMemberN)
	if !ok {
		return 0, fmt.Errorf("attribute %s is missing or not a number", name)
	}

	n, err := strconv.ParseInt(v.Value, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("parsing %s %q: %w", name, v.Value, err)
	}
	return n, nil
}

func key(id core.ClusterID) map[string]types.AttributeValue {
	return map[string]types.AttributeValue{
		attrClusterID: &types.AttributeValueMemberS{Value: id.String()},
	}
}
