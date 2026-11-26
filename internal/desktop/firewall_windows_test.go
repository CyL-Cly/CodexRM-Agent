//go:build windows

package desktop

import (
	"errors"
	"testing"
)

func TestEnsureP2PFirewallSkipsExistingRule(t *testing.T) {
	registered := false
	prompted := false
	err := ensureP2PFirewall(`C:\Apps\CodexRM\agent.exe`, func(string) bool { return true }, func(string, string) error {
		registered = true
		return nil
	}, func() bool {
		prompted = true
		return false
	})
	if err != nil || registered || prompted {
		t.Fatalf("existing rule: err=%v registered=%v prompted=%v", err, registered, prompted)
	}
}

func TestEnsureP2PFirewallExitAfterDeclinedRegistration(t *testing.T) {
	registrations := 0
	prompts := 0
	err := ensureP2PFirewall(`C:\Apps\CodexRM\agent.exe`, func(string) bool { return false }, func(string, string) error {
		registrations++
		return errors.New("cancelled")
	}, func() bool {
		prompts++
		return false
	})
	if !errors.Is(err, ErrP2PFirewallRequired) || registrations != 1 || prompts != 1 {
		t.Fatalf("declined registration: err=%v registrations=%d prompts=%d", err, registrations, prompts)
	}
}

func TestEnsureP2PFirewallRetriesUntilRuleExists(t *testing.T) {
	installed := false
	registrations := 0
	prompts := 0
	present := func(string) bool { return installed }
	err := ensureP2PFirewall(`C:\Apps\CodexRM\agent.exe`, present, func(name, executable string) error {
		registrations++
		if name != firewallRuleName(executable) {
			t.Fatalf("rule name %q does not match executable", name)
		}
		installed = registrations == 2
		return nil
	}, func() bool {
		prompts++
		return true
	})
	if err != nil || registrations != 2 || prompts != 1 {
		t.Fatalf("retry: err=%v registrations=%d prompts=%d", err, registrations, prompts)
	}
}

func TestFirewallRuleNameMatchesExecutablePath(t *testing.T) {
	a := firewallRuleName(`C:\Apps\CodexRM\Agent.exe`)
	if a != firewallRuleName(`c:\apps\codexrm\agent.exe`) {
		t.Fatal("case-insensitive executable paths produced different rules")
	}
	if a == firewallRuleName(`C:\Apps\CodexRM\other.exe`) {
		t.Fatal("different executable paths produced the same rule")
	}
}
