package gate

// A successful probe fixture supplies actual output and a terminal event.
const testProbeCompletion = "data: {\"type\":\"response.output_text.delta\",\"delta\":\"OK\",\"choices\":[{\"delta\":{\"content\":\"OK\"},\"finish_reason\":\"stop\"}]}\n\ndata: {\"type\":\"response.completed\"}\n\ndata: [DONE]\n\n"

var testLegacyRoster = []string{"bash", "edit", "glob", "grep", "read", "write", "skill", "task", "webfetch", "websearch", "question"}
