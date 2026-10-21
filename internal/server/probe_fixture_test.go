package server

// A successful probe fixture supplies actual output and a terminal event.
const fixtureProbeCompletion = "data: {\"type\":\"response.output_text.delta\",\"delta\":\"OK\",\"choices\":[{\"delta\":{\"content\":\"OK\"},\"finish_reason\":\"stop\"}]}\n\ndata: {\"type\":\"response.completed\"}\n\ndata: [DONE]\n\n"
