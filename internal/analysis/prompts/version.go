package prompts

// TemplateVersion identifies the prompt templates in this package for
// decision snapshots (issue #589). Bump it in the same change as any edit to
// prompt text, structure, or rendering that a model can observe; snapshots
// taken under different versions are then distinct decisions.
const TemplateVersion = "prompts/2026-11-09.1"
