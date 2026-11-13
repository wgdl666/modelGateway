package protocol

import (
	"bytes"
	"encoding/json"
	"errors"
	"image"
	_ "image/gif"
	_ "image/jpeg"
	_ "image/png"
	"strconv"
	"strings"

	_ "golang.org/x/image/webp"
)

// MediaRef 描述一张可单独读取的输入或成品，不包含图片字节。
type MediaRef struct {
	Index    int    `json:"index"`
	Role     string `json:"role,omitempty"`
	MIMEType string `json:"mime_type"`
	// 只读图片头得到的原始像素尺寸。受理不校验图片内容，读不出尺寸时省略。
	Width  int `json:"width,omitempty"`
	Height int `json:"height,omitempty"`
}

func inputRoles(kind string) []string {
	switch kind {
	case ClothGenProtocol, BodyProtocol, VideoProtocol:
		return []string{"image"}
	case WardrobeProtocol:
		return []string{"image", "parse_mask"}
	case OutfitProtocol:
		return []string{"person_image", "garment_image"}
	default:
		return nil
	}
}

// previewFields 保留图片为原始 JSON，参数和清单读取不进行 Base64 解码。
func previewFields(payload json.RawMessage) (map[string]json.RawMessage, []json.RawMessage, error) {
	var stored Submission
	if json.Unmarshal(payload, &stored) != nil || len(stored.Input) == 0 {
		return nil, nil, errors.New("stored input is unavailable")
	}
	var fields map[string]json.RawMessage
	if json.Unmarshal(stored.Input, &fields) != nil || fields == nil {
		return nil, nil, errors.New("stored input is unavailable")
	}
	var images []json.RawMessage
	if err := json.Unmarshal(fields["images"], &images); err != nil {
		return nil, nil, err
	}
	delete(fields, "images")
	return fields, images, nil
}

// PreviewSubmission 只提取参数、序号和 MIME；尺寸由受理时保存的清单提供。
func PreviewSubmission(kind string, payload json.RawMessage) (json.RawMessage, []MediaRef, error) {
	fields, images, err := previewFields(payload)
	if err != nil {
		return nil, nil, err
	}
	roles := inputRoles(kind)
	refs := make([]MediaRef, len(images))
	for i, raw := range images {
		var meta struct {
			MIMEType string `json:"mime_type"`
		}
		if err := json.Unmarshal(raw, &meta); err != nil {
			return nil, nil, err
		}
		refs[i] = MediaRef{Index: i, MIMEType: meta.MIMEType}
		if i < len(roles) {
			refs[i].Role = roles[i]
		}
	}
	parameters, err := json.Marshal(fields)
	return parameters, refs, err
}

// PreviewInputImage 仅解码指定图片，其他图片即使损坏也不阻断这一张的预览。
func PreviewInputImage(payload json.RawMessage, index int) (*InputImage, error) {
	_, images, err := previewFields(payload)
	if err != nil {
		return nil, err
	}
	if index < 0 || index >= len(images) {
		return nil, errors.New("input image is unavailable")
	}
	var img InputImage
	if err := DecodeJSON(images[index], &img); err != nil {
		return nil, err
	}
	if !imageMIME.MatchString(img.MIMEType) || len(img.Data) == 0 {
		return nil, errors.New("invalid image")
	}
	return &img, nil
}

// InputMediaExtra 把输入清单放进通用 extra。键缺失表示未记录输入清单，空数组表示确认没有输入。
func InputMediaExtra(refs []MediaRef) (json.RawMessage, error) {
	if refs == nil {
		refs = []MediaRef{}
	}
	return json.Marshal(struct {
		InputMedia []MediaRef `json:"input_media"`
	}{refs})
}

// InputMedia 按协议给每张输入图标角色。受理时写入账本，详情根据原始输入现场生成，二者口径一致。
func InputMedia(kind string, images []InputImage) []MediaRef {
	roles := inputRoles(kind)
	refs := make([]MediaRef, len(images))
	for i, img := range images {
		role := ""
		if i < len(roles) {
			role = roles[i]
		}
		refs[i] = MediaRef{Index: i, Role: role, MIMEType: img.MIMEType}
		if config, _, err := image.DecodeConfig(bytes.NewReader(img.Data)); err == nil {
			refs[i].Width, refs[i].Height = config.Width, config.Height
		}
	}
	return refs
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
