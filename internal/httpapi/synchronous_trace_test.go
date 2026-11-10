package httpapi

import (
	"github.com/cloudwego/hertz/pkg/app"
	"testing"
)

// 真实 ModelHub 不发送旧私有头，必须用标准上下文贯通账本；显式关联键仍保留原约定。
func TestSynchronousTraceID(t *testing.T) {
	const traceID = "1234567890abcdef1234567890abcdef"
	const parent = "00-" + traceID + "-1234567890abcdef-01"
	cases := []struct{ name, explicit, parent, want string }{
		{"w3c", "", parent, traceID},
		{"explicit wins", "legacy-trace", parent, "legacy-trace"},
		{"missing", "", "", ""},
		{"truncated", "", "00-1234-1234567890abcdef-01", ""},
		{"zero trace", "", "00-00000000000000000000000000000000-1234567890abcdef-01", ""},
		{"zero span", "", "00-" + traceID + "-0000000000000000-01", ""},
		{"invalid span", "", "00-" + traceID + "-1234567890abcdez-01", ""},
		{"invalid flags", "", "00-" + traceID + "-1234567890abcdef-zz", ""},
		{"uppercase", "", "00-1234567890ABCDEF1234567890ABCDEF-1234567890abcdef-01", ""},
		{"extra field", "", parent + "-extra", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := new(app.RequestContext)
			c.Request.Header.Set("X-Trace-ID", tc.explicit)
			c.Request.Header.Set("traceparent", tc.parent)
			if got := synchronousTraceID(c); got != tc.want {
				t.Fatalf("trace=%q want %q", got, tc.want)
			}
		})
	}
}
