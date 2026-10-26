package upstream

import (
	"bytes"
	"encoding/json"
)

// UnwrapDataLayer 上游 LiteLLM 非流式响应间歇性会多包一层 {"data":{...}}：
// Python 版 chat 路由原样透传、responses/messages 路由直接解析失败——这是已知
// 缺陷，Go 版三个非流式路径统一在此解掉。内层确有 choices 或 object 字段才解包，
// 避免误伤本就带 data 字段的正常响应。
func UnwrapDataLayer(body []byte) []byte {
	trimmed := bytes.TrimSpace(body)
	if len(trimmed) == 0 || trimmed[0] != '{' {
		return body
	}
	var outer struct {
		Data json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal(trimmed, &outer); err != nil || len(outer.Data) == 0 {
		return body
	}
	var inner struct {
		Choices json.RawMessage `json:"choices"`
		Object  string          `json:"object"`
	}
	if err := json.Unmarshal(outer.Data, &inner); err != nil {
		return body
	}
	if len(inner.Choices) == 0 && inner.Object == "" {
		return body
	}
	return outer.Data
}
