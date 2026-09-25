package core_test

import (
	"context"
	"fmt"
	"testing"

	"github.com/mateusememe/syntroph/core"
	"github.com/mateusememe/syntroph/core/skillcontracttest"
)

func TestInMemorySkillPortContract(t *testing.T) {
	skillcontracttest.RunSkillPort(t, "in-memory", func(t *testing.T) skillcontracttest.SkillPortFixture {
		t.Helper()
		generated := 0
		port, err := core.NewInMemorySkillPort([]core.SkillCatalogEntry{
			readySkillEntry(),
			{
				State: core.UnsupportedSkillPackage,
				Package: core.SkillPackage{Identity: core.SkillPackageIdentity{
					SourceID: "mattpocock",
					Name:     "diagnosing-bugs",
				}},
				Diagnostic: "scripts are not supported before SandboxPort integration",
			},
		}, nil, func() (string, error) {
			generated++
			return fmt.Sprintf("inv-generated-%d", generated), nil
		})
		if err != nil {
			t.Fatal(err)
		}
		return skillcontracttest.SkillPortFixture{
			Port:            port,
			ReadyName:       "mattpocock/code-review",
			UnsupportedName: "mattpocock/diagnosing-bugs",
			Runtime:         "codex",
		}
	})
}

func TestDeterministicRuntimePortContract(t *testing.T) {
	skillcontracttest.RunRuntimePort(t, "deterministic-fake", func(t *testing.T) skillcontracttest.RuntimePortFixture {
		t.Helper()
		bundle := prepareContractBundle(t)
		runtime := &skillcontracttest.RecordingRuntime{}
		return skillcontracttest.RuntimePortFixture{
			Port:     runtime,
			Bundle:   bundle,
			Received: runtime.LastBundle,
		}
	})
}

func prepareContractBundle(t *testing.T) core.SkillBundle {
	t.Helper()
	port, err := core.NewInMemorySkillPort([]core.SkillCatalogEntry{readySkillEntry()}, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	bundle, err := port.Prepare(context.Background(), core.SkillPrepareRequest{
		Name:         "mattpocock/code-review",
		InvocationID: "inv-runtime-contract",
		Runtime:      "codex",
		Arguments:    map[string]any{"fixed_point": "origin/main"},
	})
	if err != nil {
		t.Fatal(err)
	}
	return bundle
}
