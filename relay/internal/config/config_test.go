package config

import "testing"

// The Space carries HF_DATA_TOKEN, created by the dataset owner. HF_TOKEN stays
// supported as a fallback so local scripts and older deployments do not break,
// but the documented variable must win when both are present.
func TestDataTokenTakesPrecedence(t *testing.T) {
	t.Setenv("HF_DATA_TOKEN", "from-dataset-owner")
	t.Setenv("HF_TOKEN", "legacy")
	c := Load()
	if c.HFToken != "from-dataset-owner" {
		t.Fatalf("HFToken = %q, want the HF_DATA_TOKEN value", c.HFToken)
	}
	if !c.HFEnabled {
		t.Fatal("dataset access should be enabled when HF_DATA_TOKEN is set")
	}
}

func TestLegacyTokenStillWorks(t *testing.T) {
	t.Setenv("HF_DATA_TOKEN", "")
	t.Setenv("HF_TOKEN", "legacy")
	if c := Load(); c.HFToken != "legacy" || !c.HFEnabled {
		t.Fatalf("legacy HF_TOKEN should still enable dataset access, got %q", c.HFToken)
	}
}

func TestNoTokenDisablesDataset(t *testing.T) {
	t.Setenv("HF_DATA_TOKEN", "")
	t.Setenv("HF_TOKEN", "")
	t.Setenv("HF_DISABLED", "")
	c := Load()
	if c.HFEnabled {
		t.Fatal("dataset access must be off when no token is set")
	}
}
