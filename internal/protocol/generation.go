package protocol

// Generation is the model's per-request execution receipt. Pointer values retain
// measured zero and unknown/null separately; legacy records may omit it entirely.
type Generation struct {
	Retries          *GenerationRetries    `json:"retries,omitempty"`
	ModelVersion     *ModelVersion         `json:"model_version"`
	EffectivePrompt  *string               `json:"effective_prompt"`
	PEStatus         *string               `json:"pe_status"`
	PEFallbackReason *string               `json:"pe_fallback_reason,omitempty"`
	Parameters       *GenerationParameters `json:"parameters"`
	Timings          *GenerationTimings    `json:"timings"`
}

type ModelVersion struct {
	Checkpoint *string       `json:"checkpoint"`
	Revision   *string       `json:"revision"`
	SHA256     *string       `json:"sha256,omitempty"`
	LoRA       *ModelVersion `json:"lora,omitempty"`
	Strength   *float64      `json:"strength,omitempty"`
}

type GenerationParameters struct {
	Seed         *int64   `json:"seed"`
	Width        *int     `json:"width"`
	Height       *int     `json:"height"`
	Steps        *int     `json:"steps"`
	Guidance     *float64 `json:"guidance"`
	OutputFormat *string  `json:"output_format"`
	StageSteps   []int    `json:"stage_steps,omitempty"`
	FPS          *float64 `json:"fps,omitempty"`
	NumFrames    *int     `json:"num_frames,omitempty"`
}

type GenerationTimings struct {
	QueueMS        *float64 `json:"queue_ms"`
	PEMS           *float64 `json:"pe_ms"`
	InferenceMS    *float64 `json:"inference_ms"`
	TotalMS        *float64 `json:"total_ms"`
	GatewayQueueMS *float64 `json:"gateway_queue_ms,omitempty"`
	GatewayTotalMS *float64 `json:"gateway_total_ms,omitempty"`
}

// Counts exclude the initial attempt; a nil stage was not reported.
type RetrySummary struct {
	Count      int    `json:"count"`
	LastReason string `json:"last_reason,omitempty"`
}
type GenerationRetries struct {
	PE              *RetrySummary `json:"pe,omitempty"`
	Generation      *RetrySummary `json:"generation,omitempty"`
	GatewayDispatch *RetrySummary `json:"gateway_dispatch,omitempty"`
}
