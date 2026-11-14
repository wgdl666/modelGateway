package protocol

// 协议描述已有处理逻辑，不绑定模型版本；新增同协议模型只需登记新的 model_key。
const (
	WardrobeProtocol       = "garment_extraction"
	ClothGenProtocol       = "product_generation"
	OutfitProtocol         = "vton"
	BodyProtocol           = "body_model"
	VideoProtocol          = "outfit_video"
	HumanParseProtocol     = "human_parse"
	HumanYOLOProtocol      = "human_yolo"
	SegmentPersonProtocol  = "segment_person"
	SegmentSubjectProtocol = "segment_subject"
	LensCorrectionProtocol = "lens_correction"
	ChatProtocol           = "chat_completions"
	DecisionProtocol       = "decision"
	FaceSimProtocol        = "face_compare"
)

func MediaProtocol(kind string) bool {
	switch kind {
	case ImageGenerationProtocol, VideoGenerationProtocol, WardrobeProtocol, ClothGenProtocol, OutfitProtocol, BodyProtocol, VideoProtocol:
		return true
	default:
		return false
	}
}

func ValidProtocol(kind string) bool {
	return MediaProtocol(kind) || SynchronousKind(kind) != ""
}
