// Package series holds the Prometheus-shaped data model shared by every other
// package: label sets, label matchers and metric selectors.
//
// burnrate speaks PromQL vocabulary on purpose. SLO indicators are written as
// PromQL series selectors, incoming OTLP points are translated to Prometheus
// metric and label names, and the same selectors are emitted verbatim into the
// generated recording rules. One vocabulary keeps the in-process engine and the
// generated Prometheus rules looking at the same series.
package series

import (
	"sort"
	"strings"
)

// Label is a single name/value pair.
type Label struct {
	Name  string
	Value string
}

// Labels is a label set sorted by name. Use FromMap or Builder to construct one;
// the methods assume the slice is sorted and free of duplicate names.
type Labels []Label

// FromMap builds a sorted label set from a map. Empty values are dropped, which
// mirrors Prometheus: a label with an empty value is the same as no label.
func FromMap(m map[string]string) Labels {
	ls := make(Labels, 0, len(m))
	for k, v := range m {
		if v == "" {
			continue
		}
		ls = append(ls, Label{Name: k, Value: v})
	}
	sort.Slice(ls, func(i, j int) bool { return ls[i].Name < ls[j].Name })
	return ls
}

// Get returns the value of the named label, or "" when it is absent.
func (ls Labels) Get(name string) string {
	i := sort.Search(len(ls), func(i int) bool { return ls[i].Name >= name })
	if i < len(ls) && ls[i].Name == name {
		return ls[i].Value
	}
	return ""
}

// Map returns the label set as a fresh map.
func (ls Labels) Map() map[string]string {
	m := make(map[string]string, len(ls))
	for _, l := range ls {
		m[l.Name] = l.Value
	}
	return m
}

// String renders the label set in PromQL form, e.g. {a="1", b="2"}.
func (ls Labels) String() string {
	var b strings.Builder
	b.WriteByte('{')
	for i, l := range ls {
		if i > 0 {
			b.WriteString(", ")
		}
		b.WriteString(l.Name)
		b.WriteByte('=')
		b.WriteString(quote(l.Value))
	}
	b.WriteByte('}')
	return b.String()
}

// Key returns a compact identity string for the label set, suitable as a map
// key. Names and values are separated by bytes that cannot appear in valid
// UTF-8 label text, so distinct sets never collide.
func (ls Labels) Key() string {
	var b strings.Builder
	for _, l := range ls {
		b.WriteString(l.Name)
		b.WriteByte(0xfe)
		b.WriteString(l.Value)
		b.WriteByte(0xff)
	}
	return b.String()
}

// Builder accumulates labels and resolves name collisions the way the
// Prometheus OTLP receiver does: values for the same name are joined with ';'
// in the order they were added.
type Builder struct {
	m map[string]string
}

// NewBuilder returns an empty Builder.
func NewBuilder() *Builder {
	return &Builder{m: make(map[string]string)}
}

// Set adds a label. An existing value under the same name is extended with
// ";"+value, matching Prometheus' handling of attributes that collide after
// name sanitization.
func (b *Builder) Set(name, value string) {
	if old, ok := b.m[name]; ok {
		if value != "" {
			if old == "" {
				b.m[name] = value
			} else {
				b.m[name] = old + ";" + value
			}
		}
		return
	}
	b.m[name] = value
}

// Override replaces the value under name, dropping any accumulated value.
func (b *Builder) Override(name, value string) {
	b.m[name] = value
}

// Labels returns the sorted label set.
func (b *Builder) Labels() Labels {
	return FromMap(b.m)
}
