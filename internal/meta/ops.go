package meta

// Relay persisted-query doc_ids verified against the meta.ai bundle.
//
// The constants pair each operation name with the doc_id the browser sends in
// the {"doc_id": ..., "variables": ...} body. Only operations confirmed in the
// capture are listed; a doc_id that cannot be verified is not guessed.
const (
	// NameDocEctoHomePromptRouteQuery is the home route prompt operation.
	NameDocEctoHomePromptRouteQuery = "EctoHomePromptRouteQuery"
	// DocEctoHomePromptRouteQuery resolves the home prompt for a viewer.
	DocEctoHomePromptRouteQuery = "816384a4f0a1c70e4631528a5ff36e8e"

	// NameDocEctoMergedMessageListPreloadQuery is the conversation preload op.
	NameDocEctoMergedMessageListPreloadQuery = "EctoMergedMessageListPreloadQuery"
	// DocEctoMergedMessageListPreloadQuery preloads a conversation's messages.
	DocEctoMergedMessageListPreloadQuery = "fc1d23c614253430e896695d71e80aaf"

	// NameDocMarkConversationLastSeen is the read-receipt mutation.
	NameDocMarkConversationLastSeen = "useMarkConversationLastSeenMessageMutation"
	// DocMarkConversationLastSeen marks the last seen message of a conversation.
	DocMarkConversationLastSeen = "0b2cb3a499606dfe326807a2b8b7ee20"
)
