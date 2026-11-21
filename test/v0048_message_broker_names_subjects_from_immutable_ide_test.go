//go:build !integration

package test

import (
	"regexp"
	"strings"
	"testing"

	"github.com/pandops/flowai-team-registry/internal/team"
)

// Requirement: message-broker names subjects from immutable identifiers —
// Team appears as a subject token only in the task queue and Source intake.
// team-registry's subjects carry no team token at all.

func TestTeamRegistrySubjectsCarryNoTeamToken(t *testing.T) {
	e := newEnv(t)
	exerciseAllOperations(t, e)
	published := regexp.MustCompile(`^team-registry\.(event|audit)\.[a-z]+(_[a-z]+)*\.pub$`)
	msgs := e.publish(t)
	if len(msgs) == 0 {
		t.Fatal("nothing published")
	}
	for _, m := range msgs {
		if strings.HasPrefix(m.Subject, "flowai.") || !published.MatchString(m.Subject) {
			t.Fatalf("published subject %q does not follow team-registry.<item>.<kind>.pub with no team token", m.Subject)
		}
	}
	served := regexp.MustCompile(`^team-registry\.(admin|service)\.[a-z]+(-[a-z]+)*\.sub$`)
	if len(team.Subjects()) == 0 {
		t.Fatal("team-registry serves no subject")
	}
	for _, s := range team.Subjects() {
		if !served.MatchString(s) {
			t.Fatalf("served subject %q is not team-registry.<class>.<operation>.sub with a hyphenated operation", s)
		}
	}
}
