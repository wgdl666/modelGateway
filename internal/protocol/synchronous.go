package protocol

const (
	HumanParseModel     = "Vmind_human_parse_v1.0"
	HumanYOLOModel      = "human-yolo"
	SegmentPersonModel  = "Vmind_segment_person_bria_v1.0"
	SegmentSubjectModel = "Vmind_segment_subject_bria_v1.0"
	LensCorrectionModel = "lens-distortion-correction"
	DreamModel          = "Vmind_dream_v1"
	DecisionModel       = "Vmind_decision_v1"
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
	default:
		return ""
	}
}

// SynchronousKind 只扩大登记范围，图像和视频任务接口仍使用 ValidModelKey。
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
