// Package envx resolves the environment of a claude session from layered
// sources — the container's own environment, the devcontainer's remoteEnv,
// cld's defaults, the user's cld.yaml, and cld's managed keys — keeping track
// of which layer each value came from so the result can be explained.
//
// It is deliberately free of any docker or config dependency: layers go in,
// resolved variables come out.
package envx

import (
	"sort"
	"strings"
)

// OriginContainer marks a variable the container already had (its image ENV
// and the devcontainer's containerEnv). Those are inherited by every exec, so
// they are reported but never re-sent.
const OriginContainer = "container"

// Var is one resolved variable and the layer that decided its value.
type Var struct {
	Key    string
	Value  string
	Origin string
	// Unset means a layer asked for the inherited value to be removed. The
	// variable cannot simply be omitted from the exec environment — it would
	// still be inherited — so callers drop it with a shell `unset`.
	Unset bool
}

// Layer is one source of variables. A nil value means "remove this variable",
// which is distinct from an empty string ("set it to empty").
type Layer struct {
	Origin string
	Vars   map[string]*string
}

// Lookup resolves a ${ns:name} reference. ns is empty for a plain ${name}.
// Resolve answers "" and "containerEnv" itself from the layers resolved so
// far; every other namespace is delegated to the Lookup passed to it, which is
// where a caller plugs in its own sources (e.g. the daemon's own environment).
type Lookup func(ns, name string) (string, bool)

// Result is the resolved environment, ordered by key.
type Result struct {
	Vars []Var
}

// Resolve applies layers in order over the container's own environment. Values
// may reference variables resolved by EARLIER layers (or the container) with
// ${NAME}; siblings within the same layer are deliberately invisible to each
// other, so the result does not depend on YAML map ordering.
func Resolve(base []string, extra Lookup, layers ...Layer) Result {
	vars := make(map[string]Var, len(base))
	for _, kv := range base {
		k, v, ok := strings.Cut(kv, "=")
		if !ok || k == "" {
			continue
		}
		vars[k] = Var{Key: k, Value: v, Origin: OriginContainer}
	}

	for _, l := range layers {
		// Expand against a snapshot: within one layer, no key can see another.
		look := lookupIn(vars, extra)
		next := make([]Var, 0, len(l.Vars))
		for _, k := range sortedKeys(l.Vars) {
			p := l.Vars[k]
			if p == nil {
				next = append(next, Var{Key: k, Origin: l.Origin, Unset: true})
				continue
			}
			next = append(next, Var{Key: k, Value: Expand(*p, look), Origin: l.Origin})
		}
		for _, v := range next {
			vars[v.Key] = v
		}
	}

	out := make([]Var, 0, len(vars))
	for _, v := range vars {
		out = append(out, v)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Key < out[j].Key })
	return Result{Vars: out}
}

// lookupIn answers from the variables resolved so far, delegating unknown
// namespaces to extra. An unset variable reads as absent, exactly as it will
// inside the container.
func lookupIn(vars map[string]Var, extra Lookup) Lookup {
	return func(ns, name string) (string, bool) {
		switch ns {
		case "", "containerEnv":
			v, ok := vars[name]
			if !ok || v.Unset {
				return "", false
			}
			return v.Value, true
		default:
			if extra == nil {
				return "", false
			}
			return extra(ns, name)
		}
	}
}

// Overrides is the KEY=VALUE list to hand to a docker exec: every variable a
// layer set. Variables the container already had are left out — the exec
// inherits them — and removed ones cannot be expressed here (see Unset).
func (r Result) Overrides() []string {
	out := make([]string, 0, len(r.Vars))
	for _, v := range r.Vars {
		if v.Origin == OriginContainer || v.Unset {
			continue
		}
		out = append(out, v.Key+"="+v.Value)
	}
	return out
}

// Unset is the list of variables a layer removed. A docker exec can add and
// replace variables but never drop an inherited one, so the caller has to
// unset these in the command itself.
func (r Result) Unset() []string {
	out := []string{}
	for _, v := range r.Vars {
		if v.Unset {
			out = append(out, v.Key)
		}
	}
	return out
}

// Value returns the effective value of a variable, and whether it is set.
func (r Result) Value(key string) (string, bool) {
	for _, v := range r.Vars {
		if v.Key == key {
			return v.Value, !v.Unset
		}
	}
	return "", false
}

// Expand resolves ${...} references in s.
//
//	${NAME}              the value, or "" when unset
//	${NAME:-fallback}    fallback when NAME is unset or empty (as in a shell)
//	${ns:NAME}           a namespaced source, e.g. ${containerEnv:PATH}
//	$$                   a literal "$"
//
// A "$" not followed by "{" is literal, so values that merely contain the
// character (a password, say) need no escaping. An unterminated "${" is
// likewise left as written rather than swallowing the rest of the value.
// Fallbacks are expanded in turn, so ${A:-${B}} works.
func Expand(s string, look Lookup) string {
	var b strings.Builder
	for i := 0; i < len(s); {
		if s[i] == '$' && i+1 < len(s) {
			switch s[i+1] {
			case '$':
				b.WriteByte('$')
				i += 2
				continue
			case '{':
				end, ok := closeBrace(s, i+2)
				if !ok {
					b.WriteString(s[i:])
					return b.String()
				}
				b.WriteString(expandRef(s[i+2:end], look))
				i = end + 1
				continue
			}
		}
		b.WriteByte(s[i])
		i++
	}
	return b.String()
}

// expandRef resolves the inside of a ${...}: an optional "ns:" prefix, a name,
// and an optional ":-fallback".
func expandRef(ref string, look Lookup) string {
	name, fallback := ref, ""
	has_fallback := false
	if i := indexFallback(ref); i >= 0 {
		name, fallback, has_fallback = ref[:i], ref[i+2:], true
	}

	ns := ""
	if i := strings.IndexByte(name, ':'); i >= 0 {
		ns, name = name[:i], name[i+1:]
	}

	v, ok := look(ns, name)
	if (!ok || v == "") && has_fallback {
		return Expand(fallback, look)
	}
	return v
}

// closeBrace finds the "}" that closes a reference opened before i, so a
// nested reference inside a fallback does not end it early.
func closeBrace(s string, i int) (int, bool) {
	depth := 0
	for ; i < len(s); i++ {
		switch s[i] {
		case '{':
			depth++
		case '}':
			if depth == 0 {
				return i, true
			}
			depth--
		}
	}
	return 0, false
}

// indexFallback finds the ":-" that separates a name from its fallback,
// ignoring one nested inside a reference of its own.
func indexFallback(s string) int {
	depth := 0
	for i := 0; i < len(s)-1; i++ {
		switch s[i] {
		case '{':
			depth++
		case '}':
			depth--
		case ':':
			if depth == 0 && s[i+1] == '-' {
				return i
			}
		}
	}
	return -1
}

// ValidKey reports whether k can be an environment variable name.
func ValidKey(k string) bool {
	if k == "" {
		return false
	}
	for i := 0; i < len(k); i++ {
		c := k[i]
		ok := c == '_' ||
			c >= 'A' && c <= 'Z' ||
			c >= 'a' && c <= 'z' ||
			(i > 0 && c >= '0' && c <= '9')
		if !ok {
			return false
		}
	}
	return true
}

func sortedKeys(m map[string]*string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
