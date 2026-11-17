package parse

import (
	"sort"
	"strings"
	"testing"
)

func TestRubySuperIsACall(t *testing.T) {
	res := parse(t, "a.rb", `class Request < Base
  def initialize(env)
    super
    @env = env
  end

  def run(a)
    super(a, 1)
  end

  def self.build
    super()
  end

  def other
    super.merge(1)
  end

  define_method(:dyn) { super() }
end
`)
	var got []string
	for _, r := range refRows(res) {
		if strings.Contains(r, "recv=super/super") {
			got = append(got, r)
		}
	}
	wantList(t, "super calls", got, []string{
		"reference.call build recv=super/super args=?",
		"reference.call initialize recv=super/super args=?",
		// `super.merge(1)` is a call of merge on the result; it is not a super call
		// of other.
		"reference.call merge recv=super/super args=1",
		"reference.call run recv=super/super args=?",
	})
}

func TestRubyMixinStatementIsNotACall(t *testing.T) {
	res := parse(t, "a.rb", `module M
  include Comparable
  extend Forwardable
  prepend Rack::Hooks
  include some_helper
  host.include(Other)
  thing.extend(Mod)
end
`)
	var calls, supers []string
	for _, r := range res.References {
		switch {
		case strings.HasPrefix(r.Kind, "reference.call"):
			calls = append(calls, r.Name+"/"+r.Receiver)
		case strings.HasPrefix(r.Kind, "reference.implementation"):
			supers = append(supers, r.Name)
		}
	}
	// A bare mixin statement (even `include some_helper`, which names no module)
	// is no call; one with a receiver is still a call.
	sort.Strings(calls)
	wantList(t, "calls", calls, []string{"extend/thing", "include/host"})
	wantList(t, "supertypes", supers, []string{"Comparable", "Forwardable", "Hooks"})
}
