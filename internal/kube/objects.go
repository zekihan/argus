package kube

import (
	"fmt"
	"math"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// Condition is a status condition. Message is deliberately not decoded:
// controller and provider messages can carry secret material.
type Condition struct {
	Type               string `json:"type"`
	Status             string `json:"status"`
	Reason             string `json:"reason"`
	LastTransitionTime string `json:"lastTransitionTime"`
	ObservedGeneration *int64 `json:"observedGeneration,omitempty"`
}

// FindCondition returns the first condition of the given type.
func FindCondition(conditions []Condition, kind string) (Condition, bool) {
	for _, c := range conditions {
		if c.Type == kind {
			return c, true
		}
	}
	return Condition{}, false
}

// ConditionTrue reports whether the condition is present with status True.
func ConditionTrue(conditions []Condition, kind string) bool {
	c, ok := FindCondition(conditions, kind)
	return ok && c.Status == "True"
}

var rfc3339 = regexp.MustCompile(`^\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}(\.\d+)?(Z|[+-](0\d|1\d|2[0-3]):[0-5]\d)$`)

// ParseTime parses an RFC 3339 timestamp that carries a zone; naive or
// otherwise malformed values are errors.
func ParseTime(value string) (time.Time, error) {
	if !rfc3339.MatchString(value) {
		return time.Time{}, fmt.Errorf("malformed timestamp")
	}
	parsed, err := time.Parse(time.RFC3339Nano, value)
	if err != nil {
		return time.Time{}, fmt.Errorf("malformed timestamp")
	}
	return parsed.UTC(), nil
}

// OwnedBy reports whether meta has a controller owner of the kind and UID.
func OwnedBy(meta Metadata, kind, uid string) bool {
	for _, owner := range meta.Owners {
		if owner.Controller && owner.Kind == kind && owner.UID != "" && owner.UID == uid {
			return true
		}
	}
	return false
}

var quantity = regexp.MustCompile(`^([+-]?[0-9.]+(?:[eE][+-]?[0-9]+)?)(n|u|m|k|M|G|T|P|E|Ki|Mi|Gi|Ti|Pi|Ei)?$`)

var suffixes = map[string]float64{"": 1, "n": 1e-9, "u": 1e-6, "m": 1e-3, "k": 1e3, "M": 1e6, "G": 1e9, "T": 1e12, "P": 1e15, "E": 1e18,
	"Ki": 1 << 10, "Mi": 1 << 20, "Gi": 1 << 30, "Ti": 1 << 40, "Pi": 1 << 50, "Ei": 1 << 60}

// Quantity parses a Kubernetes resource quantity into base units.
func Quantity(value string) (float64, error) {
	match := quantity.FindStringSubmatch(strings.TrimSpace(value))
	if match == nil {
		return 0, fmt.Errorf("malformed quantity")
	}
	number, err := strconv.ParseFloat(match[1], 64)
	if err != nil || math.IsNaN(number) || math.IsInf(number, 0) {
		return 0, fmt.Errorf("malformed quantity")
	}
	return number * suffixes[match[2]], nil
}
