package consumer

// noteText reads a note/task body as sent to Twenty: bodyV2.markdown (Twenty v1.x rejects the old "body").
func noteText(body map[string]any) string {
	switch v := body["bodyV2"].(type) {
	case map[string]any:
		s, _ := v["markdown"].(string)
		return s
	}
	s, _ := body["body"].(string)
	return s
}
