package main

import (
	"fmt"
	"strings"

	meshcore "github.com/OwlShack/meshcore-go"
)

// FloodScope is the region a flood we originate is scoped to: the level
// above's choice, unscoped, or one region. Same values as OwlShack.
type FloodScope string

const (
	ScopeInherit      FloodScope = "inherit"
	ScopeEverywhere   FloodScope = "everywhere"
	scopeRegionPrefix            = "region:"
)

// regionName is the region's name, ok=false for inherit and everywhere.
func (s FloodScope) regionName() (string, bool) {
	return strings.CutPrefix(string(s), scopeRegionPrefix)
}

// meshRegion is what a packet is scoped with; nil sends unscoped.
func (s FloodScope) meshRegion() *meshcore.Region {
	name, ok := s.regionName()
	if !ok {
		return nil
	}
	return meshcore.NewRegion(name)
}

// resolveScope walks from the most specific level up and takes the first that
// does not inherit; unset inherits, and the top falls back to unscoped.
func resolveScope(levels ...FloodScope) FloodScope {
	for _, s := range levels {
		if s != ScopeInherit && s != "" {
			return s
		}
	}
	return ScopeEverywhere
}

// validate checks the value's form. allowInherit is false at the top level,
// where there is nothing above to inherit from.
func (s FloodScope) validate(allowInherit bool) error {
	switch s {
	case "", ScopeEverywhere:
		return nil
	case ScopeInherit:
		if allowInherit {
			return nil
		}
		return fmt.Errorf("there is no level above to inherit a region from")
	}
	name, ok := s.regionName()
	if !ok {
		return fmt.Errorf("floodScope %q must be inherit, everywhere or region:<name>", s)
	}
	return validateRegionName(name)
}

// validateRegionName takes the firmware's name characters, except "$" (a
// private region has no key to derive) and "#" (added when the key is derived).
func validateRegionName(name string) error {
	if name == "" {
		return fmt.Errorf("region name is empty")
	}
	if len(name) > meshcore.MaxRegionName {
		return fmt.Errorf("region name %q is longer than %d bytes", name, meshcore.MaxRegionName)
	}
	for i := 0; i < len(name); i++ {
		if c := name[i]; c == '$' || c == '#' || !meshcore.IsValidRegionNameChar(c) {
			return fmt.Errorf("region name %q has a character the firmware does not take: %q", name, c)
		}
	}
	return nil
}
