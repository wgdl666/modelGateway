package protocol

import (
	"encoding/json"
	"errors"
	"strconv"
	"strings"
)

// MediaRef 描述一张可单独读取的输入或成品，不包含图片字节。
type MediaRef struct {
	Index    int    `json:"index"`
	Role     string `json:"role,omitempty"`
	MIMEType string `json:"mime_type"`
}

func inputRoles(model string) []string {
	switch model {
	case ClothGenModel, BodyModel, VideoModel:
		return []string{"image"}
	case WardrobeModel:
		return []string{"image", "parse_mask"}
	case OutfitModel:
		return []string{"person_image", "garment_image"}
	default:
		return nil
	}
}

// PreviewSubmission 从已保存的受理正文取出业务字段和图片。图片字节只留给按张读取的媒体接口。
func PreviewSubmission(model string, payload json.RawMessage) (parameters json.RawMessage, refs []MediaRef, images []InputImage, err error) {
	var stored Submission
	if json.Unmarshal(payload, &stored) != nil || len(stored.Input) == 0 {
		return nil, nil, nil, errors.New("stored input is unavailable")
	}
	fields, images, err := DecodeInput(stored.Input)
	if err != nil {
		return nil, nil, nil, err
	}
	parameters, err = json.Marshal(fields)
	if err != nil {
		return nil, nil, nil, err
	}
	roles := inputRoles(model)
	refs = make([]MediaRef, len(images))
	for i, image := range images {
		role := ""
		if i < len(roles) {
			role = roles[i]
		}
		refs[i] = MediaRef{Index: i, Role: role, MIMEType: image.MIMEType}
	}
	return parameters, refs, images, nil
}

// OutputMedia 只列出键名能对应到图片或视频的成品。其他对象引用仍留在 result 里。
func OutputMedia(files []Object) []MediaRef {
	refs := make([]MediaRef, 0, len(files))
	for i, file := range files {
		mime := OutputMIME(file.Key)
		if mime == "" {
			continue
		}
		refs = append(refs, MediaRef{Index: i, MIMEType: mime})
	}
	return refs
}

func OutputMIME(key string) string {
	switch {
	case strings.HasSuffix(key, ".jpg"), strings.HasSuffix(key, ".jpeg"):
		return "image/jpeg"
	case strings.HasSuffix(key, ".png"):
		return "image/png"
	case strings.HasSuffix(key, ".webp"):
		return "image/webp"
	case strings.HasSuffix(key, ".mp4"):
		return "video/mp4"
	default:
		return ""
	}
}

// MatchesOutputKey 确认成品引用就是这次请求发布的那一张，避免管理接口读取桶里的其他对象。
func MatchesOutputKey(id string, index int, key string) bool {
	if !ValidID(id) || index < 0 || key == "" || strings.Contains(key, "..") {
		return false
	}
	prefix := "outputs/" + id + "/"
	name, ok := strings.CutPrefix(key, prefix)
	if !ok || name == "" || strings.Contains(name, "/") {
		return false
	}
	number, _, ok := strings.Cut(name, ".")
	return ok && number == strconv.Itoa(index) && OutputMIME(name) != ""
}
