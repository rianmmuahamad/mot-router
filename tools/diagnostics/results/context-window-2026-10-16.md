# Context window check — 16 October 2026 (Asia/Jakarta)

Measured through Mot Router on the VPS, using explicit `opencode-zen/` model prefixes and a temporary diagnostic router key. No customer account quota was used. The temporary key was deleted and its absence verified.

| Model | Actual prompt tokens | Completion tokens | Outcome |
| --- | ---: | ---: | --- |
| deepseek-v4.1-flash-free | 1,004,999 | 81 | HTTP 200; stream completed; all three markers recalled; 40.00 seconds |
| glm-5.3-flash-free | unavailable | unavailable | Small calibration request returned HTTP 429: Endpoint is unavailable; no large request sent |

DeepSeek's small calibrations reported 1,681 and 2,705 prompt tokens. A synthetic repeated-text payload was scaled using the observed token slope, with distinct markers at the beginning, middle and end. The large request was approximately 2 MB. Usage reports are cumulative and were not added repeatedly. These measurements prove acceptance of more than one million input tokens on this path at the time of testing; they do not establish the exact backend upper boundary or general long-document retrieval quality.

Both official model configurations specify `text_config.max_position_embeddings = 1048576`:

- https://huggingface.co/deepseek-ai/DeepSeek-V4.1-Flash/raw/main/config.json
- https://huggingface.co/zai-org/GLM-5.3-Flash/raw/main/config.json

Model architecture capacity is distinct from provider deployment limits. The local OpenCode cache lists the free DeepSeek entry at 200,000 context tokens, despite the successful larger backend request. GLM's usable Zen free-tier window remains unverified because of the availability error; HTTP 429 in this test is not a context-length rejection.

Raw results: `context-window-2026-10-16.json`. Probe: `../context_window_probe.py`.
