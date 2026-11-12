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

// PreviewSubmission 按受理时保存的协议解释图片角色，登记变更不能重写历史预览。
// 图片字节只留给按张读取的媒体接口。
func PreviewSubmission(kind string, payload json.RawMessage) (parameters json.RawMessage, refs []MediaRef, images []InputImage, err error) {
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
	return parameters, InputMedia(kind, images), images, nil
}

// InputMediaExtra 把输入清单放进通用 extra。键缺失表示迁移前的请求，空数组表示确认没有输入。
func InputMediaExtra(refs []MediaRef) (json.RawMessage, error) {
	if refs == nil {
		refs = []MediaRef{}
	}
	return json.Marshal(struct {
		InputMedia []MediaRef `json:"input_media"`
	}{refs})
}

// InputMedia 按协议给每张输入图标角色。受理时写入账本，详情对迁移前的请求现场生成，二者口径一致。
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
