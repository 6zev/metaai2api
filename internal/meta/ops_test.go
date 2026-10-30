package meta

import "testing"

func TestOps(t *testing.T) {
	want := map[string]string{
		"EctoHomePromptRouteQuery":                   "816384a4f0a1c70e4631528a5ff36e8e",
		"EctoMergedMessageListPreloadQuery":          "fc1d23c614253430e896695d71e80aaf",
		"useMarkConversationLastSeenMessageMutation": "0b2cb3a499606dfe326807a2b8b7ee20",
	}
	got := map[string]string{
		"EctoHomePromptRouteQuery":                   DocEctoHomePromptRouteQuery,
		"EctoMergedMessageListPreloadQuery":          DocEctoMergedMessageListPreloadQuery,
		"useMarkConversationLastSeenMessageMutation": DocMarkConversationLastSeen,
	}
	for op, docID := range want {
		if got[op] != docID {
			t.Errorf("%s = %q, want %q", op, got[op], docID)
		}
		if len(got[op]) != 32 {
			t.Errorf("%s doc_id length = %d, want 32 hex chars", op, len(got[op]))
		}
	}
	if NameDocEctoHomePromptRouteQuery != "EctoHomePromptRouteQuery" {
		t.Errorf("op name mismatch: %q", NameDocEctoHomePromptRouteQuery)
	}
	if NameDocMarkConversationLastSeen != "useMarkConversationLastSeenMessageMutation" {
		t.Errorf("op name mismatch: %q", NameDocMarkConversationLastSeen)
	}
}
