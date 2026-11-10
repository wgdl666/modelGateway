package protocol

const (
	HumanParseModel = "Vmind_human_parse_v1.0"
	// 模型键与 ModelHub 保持一致，模型服务的 HTTP 路径仍使用原协议名称。
	HumanYOLOModel      = "Vmind_human_yolo_v1.0"
	SegmentPersonModel  = "Vmind_segment_person_bria_v1.0"
	SegmentSubjectModel = "Vmind_segment_subject_bria_v1.0"
	LensCorrectionModel = "Vmind_lens_distortion_correction_v1.0"
	DreamModel          = "Vmind_dream_v1"
	DecisionModel       = "Vmind_decision_v1"
	FaceSimModel        = "facesim"
)

const (
	SyncVision   = "vision"
	SyncDream    = "dream"
	SyncDecision = "decision"
)

// VisionService 对应 G6 聚合探活的服务名。分人和分主体共用同一组执行副本。
func VisionService(model string) string {
	switch model {
	case HumanParseModel:
		return "human-parser"
	case HumanYOLOModel:
		return "human-yolo"
	case SegmentPersonModel, SegmentSubjectModel:
		return "segment-person"
	case LensCorrectionModel:
		return "lens_distortion_correction"
	case FaceSimModel:
		return "facesim"
	default:
		return ""
	}
}

// SynchronousKind 选择响应协议；同步模型与生成任务共用登记和容量账本。
func SynchronousKind(model string) string {
	switch model {
	case DreamModel:
		return SyncDream
	case DecisionModel:
		return SyncDecision
	}
	if VisionService(model) != "" {
		return SyncVision
	}
	return ""
}

// SynchronousModels 用于把连接内执行与 MQ 执行分开，避免同一请求被两个执行器重复派发。
func SynchronousModels() []string {
	return []string{HumanParseModel, HumanYOLOModel, SegmentPersonModel, SegmentSubjectModel,
		LensCorrectionModel, DreamModel, DecisionModel, FaceSimModel}
}
