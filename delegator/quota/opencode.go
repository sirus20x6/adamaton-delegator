package quota

// GetOpenCodeUsage returns the placeholder usage entry for OpenCode. OpenCode
// runs on local models with no upstream quota, so we always emit a zeroed
// "unlimited / no quota" record. Mirrors the inline opencode entry the TS
// builds in getAllAgentUsage.
func GetOpenCodeUsage(_ int) (*OpenCodeUsage, error) {
	return &OpenCodeUsage{
		Sessions:     0,
		InputTokens:  0,
		OutputTokens: 0,
		Model:        "local",
	}, nil
}
