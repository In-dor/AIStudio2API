package aistudio

import (
	"slices"
	"strings"
)

// nativePrefillModels 是接受以 model 轮结尾请求的规范模型 ID，gemma- 前缀的模型同样接受，其余模型的末尾 assistant 轮按续写配置改写
var nativePrefillModels = map[string]bool{
	"gemini-3.1-pro-preview":      true,
	"gemini-pro-latest":           true,
	"gemini-3.1-flash-lite":       true,
	"gemini-3-flash-preview":      true,
	"gemini-omni-1.1-flash":       true,
	"gemini-nano-banana-2.1":      true,
	"gemini-3.1-flash-image":      true,
	"gemini-3.1-flash-lite-image": true,
	"gemini-3-pro-image":          true,
	"gemini-2.5-flash-image":      true,
}

// rewritesAssistantTail 判断末尾 assistant 轮是否改写：ContinuationRewrite 打开且模型不接受以 model 轮结尾的请求
func (s *PooledService) rewritesAssistantTail(modelID string) bool {
	return s.ContinuationRewrite && !nativePrefillModels[modelID] && !strings.HasPrefix(modelID, "gemma-")
}

// normalizeTurns 在 rewrite 时改写末尾连续 assistant 轮：prompt 非空时正文作为续写前缀连同 prompt 写入系统指令，为空时正文作为最后一轮 user；没有对话内容时以系统提示作为 user 轮
func normalizeTurns(system []string, contents []Content, rewrite bool, prompt string) ([]string, []Content) {
	if rewrite {
		tail := len(contents)
		for tail > 0 && contents[tail-1].Role == RoleAssistant {
			tail--
		}
		prefix := assistantPrefix(contents[tail:])
		contents = contents[:tail]
		switch {
		case strings.TrimSpace(prefix) == "":
		case prompt == "":
			contents = append(slices.Clip(contents), Content{Role: RoleUser, Parts: []Part{{Text: prefix}}})
		default:
			instruction := prompt + "\n<assistant_prefix>" + prefix + "</assistant_prefix>"
			if len(system) > 0 {
				instruction = "\n" + instruction
			}
			system = appendSystem(system, instruction)
		}
	}
	if len(contents) == 0 && strings.TrimSpace(SystemText(system)) != "" {
		return nil, []Content{{Role: RoleUser, Parts: []Part{{Text: SystemText(system)}}}}
	}
	return system, contents
}

// SystemText 返回系统指令各段文本按顺序连接的结果
func SystemText(system []string) string {
	return strings.Join(system, "")
}

// appendSystem 把文本追加到系统指令最后一段，没有系统指令时新建一段
func appendSystem(system []string, text string) []string {
	if len(system) == 0 {
		return []string{text}
	}
	system = slices.Clone(system)
	system[len(system)-1] += text
	return system
}

// assistantPrefix 按顺序拼接 assistant 轮的正文与代码执行内容
func assistantPrefix(contents []Content) string {
	var prefix strings.Builder
	block := func(text string) {
		if prefix.Len() > 0 && !strings.HasSuffix(prefix.String(), "\n") {
			prefix.WriteString("\n")
		}
		prefix.WriteString(text + "\n")
	}
	for _, content := range contents {
		for _, part := range content.Parts {
			switch {
			case part.Thought:
			case part.Text != "":
				prefix.WriteString(part.Text)
			case part.ExecutableCode != nil:
				block("```" + strings.ToLower(part.ExecutableCode.Language) + "\n" + strings.TrimSuffix(part.ExecutableCode.Code, "\n") + "\n```")
			case part.CodeExecutionResult != nil:
				block("```text\n" + strings.TrimSuffix(part.CodeExecutionResult.Output, "\n") + "\n```")
			}
		}
	}
	return prefix.String()
}

func partHasPayload(part Part) bool {
	return part.payload()
}

func needsThoughtSignatureSanitize(contents []Content) bool {
	for _, content := range contents {
		if len(content.Parts) == 0 {
			return true
		}
		for _, part := range content.Parts {
			if !partHasPayload(part) {
				return true
			}
			if part.FunctionCall != nil {
				if part.ThoughtSignature != "skip_thought_signature_validator" ||
					part.FunctionCall.ThoughtSignature != "skip_thought_signature_validator" {
					return true
				}
			} else if part.ThoughtSignature != "" {
				return true
			}
		}
	}
	return false
}

// sanitizeThoughtSignatures 清理对话历史中的思考签名与空签名块，避免上游 400 签名校验错误
func sanitizeThoughtSignatures(contents []Content) []Content {
	if !needsThoughtSignatureSanitize(contents) {
		return contents
	}
	cleanedContents := make([]Content, 0, len(contents))
	for _, content := range contents {
		cleanedParts := make([]Part, 0, len(content.Parts))
		for _, part := range content.Parts {
			// 如果该 Part 没有任何实质内容（仅作为签名占位符），直接丢弃
			if !partHasPayload(part) {
				continue
			}
			if part.FunctionCall != nil {
				part.ThoughtSignature = "skip_thought_signature_validator"
				call := *part.FunctionCall
				call.ThoughtSignature = "skip_thought_signature_validator"
				part.FunctionCall = &call
			} else {
				part.ThoughtSignature = ""
			}
			cleanedParts = append(cleanedParts, part)
		}
		if len(cleanedParts) > 0 {
			content.Parts = cleanedParts
			cleanedContents = append(cleanedContents, content)
		}
	}
	return cleanedContents
}

