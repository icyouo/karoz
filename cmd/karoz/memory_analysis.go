package main

import (
	"context"
	"strings"
	"unicode"
)

// memoryRetrievalQueryFor stays on the first-token path, so it must be cheap
// and deterministic. The memory index is lexical; using the raw user text is
// both the honest query representation and avoids spending a model call just
// to decide whether to run that lexical lookup.
//
// The context and agent parameters remain for call-site compatibility. They
// are deliberately not consulted: a cancelled/slow side-channel cannot delay
// or suppress the current turn's memory retrieval.
func (a *app) memoryRetrievalQueryFor(_ context.Context, _ Agent, userText string) string {
	text := strings.TrimSpace(userText)
	if text == "" {
		return ""
	}
	// Explicit references to prior context win over the cheap skip filter, so
	// a short request such as "记得吗" still retrieves matching memory.
	if memoryMessageHasCue(text) {
		return text
	}
	if memoryMessageSkipsRetrieval(text) {
		return ""
	}
	return text
}

// memoryCueTerms are explicit requests for prior context. Chinese terms are
// case-free; English terms match case-insensitively.
var memoryCueTerms = []string{"记住", "记得", "上次", "之前", "以前", "remember", "recall", "previously", "last time"}

func memoryMessageHasCue(text string) bool {
	lower := strings.ToLower(text)
	for _, cue := range memoryCueTerms {
		if strings.Contains(lower, cue) {
			return true
		}
	}
	return false
}

// memoryMessageSkipsRetrieval is the inexpensive pre-filter for inputs that
// cannot usefully benefit from long-term lexical recall.
func memoryMessageSkipsRetrieval(text string) bool {
	if strings.HasPrefix(text, "/") {
		return true
	}
	if strings.HasPrefix(strings.ToLower(text), "selected: ") {
		return true
	}
	return memoryWordCount(text) < 3
}

// memoryWordCount counts whitespace-delimited words, treating each CJK rune as
// one word so Chinese messages are not misread as trivially short.
func memoryWordCount(text string) int {
	count := 0
	inWord := false
	for _, r := range text {
		switch {
		case unicode.IsSpace(r):
			inWord = false
		case unicode.Is(unicode.Han, r) || unicode.Is(unicode.Hiragana, r) || unicode.Is(unicode.Katakana, r) || unicode.Is(unicode.Hangul, r):
			count++
			inWord = false
		default:
			if !inWord {
				count++
				inWord = true
			}
		}
	}
	return count
}
