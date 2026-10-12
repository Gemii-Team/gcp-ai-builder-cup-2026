package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

// The fixtures of a kb_answer are written by hand, and the product repairs a bad answer
// silently: it drops a marker that names no citation, renumbers the rest and shows the
// chunk's own first words in place of a quote the chunk does not hold. A typo in a
// fixture would therefore change what the screen says without any failure, so the
// replay holds a kb_answer to what a model is told to write (knowledge/prompt.go): the
// loader refuses an answer whose markers and citations disagree, and a request whose
// chunk does not hold the words of a quote is an unmatched one.

// kbAnswerSchema is the schema name of the answer call.
const kbAnswerSchema = "kb_answer"

// citationMarker is a citation marker of an answer's text, [n].
var citationMarker = regexp.MustCompile(`\[(\d+)\]`)

// kbAnswer is the content of a kb_answer response.
type kbAnswer struct {
	markdown  string
	citations []kbCitation
}

type kbCitation struct{ chunkID, quote string }

// readKBAnswer reads the answer out of a message's content: its JSON written as an
// object, or as the string the wire carries.
func readKBAnswer(content any) (kbAnswer, error) {
	if s, ok := content.(string); ok {
		tree, err := decodeTree([]byte(s))
		if err != nil {
			return kbAnswer{}, fmt.Errorf("kb_answer response: message.content is not JSON: %w", err)
		}
		content = tree
	}
	obj, ok := content.(map[string]any)
	if !ok {
		return kbAnswer{}, errors.New("kb_answer response: message.content must be the answer, a JSON object with answer_markdown and citations")
	}
	var a kbAnswer
	if a.markdown, ok = obj["answer_markdown"].(string); !ok {
		return kbAnswer{}, errors.New("kb_answer response: answer_markdown must be a string")
	}
	list, ok := obj["citations"].([]any)
	if !ok {
		return kbAnswer{}, errors.New("kb_answer response: citations must be a list (an empty one for a not-found answer)")
	}
	for i, c := range list {
		cm, _ := c.(map[string]any)
		id, _ := cm["chunk_id"].(string)
		quote, _ := cm["quote"].(string)
		if id == "" {
			return kbAnswer{}, fmt.Errorf("kb_answer response: citation %d has no chunk_id", i+1)
		}
		if strings.TrimSpace(quote) == "" {
			return kbAnswer{}, fmt.Errorf("kb_answer response: citation %d has no quote", i+1)
		}
		a.citations = append(a.citations, kbCitation{chunkID: id, quote: quote})
	}
	return a, nil
}

// check refuses an answer whose text and citations disagree: a marker outside 1..len
// (citations), a citation no marker names, or citations under no text at all. An empty
// answer with no citation is the not-found one.
func (a kbAnswer) check() error {
	if strings.TrimSpace(a.markdown) == "" {
		if len(a.citations) > 0 {
			return fmt.Errorf("kb_answer response: answer_markdown is empty and the answer has %d citation(s); the empty answer is the not-found one and has none", len(a.citations))
		}
		return nil
	}
	used := make([]bool, len(a.citations)+1)
	for _, m := range citationMarker.FindAllStringSubmatch(a.markdown, -1) {
		n, err := strconv.Atoi(m[1])
		if err != nil || n < 1 || n > len(a.citations) {
			return fmt.Errorf("kb_answer response: answer_markdown has the marker [%s], which names no citation (the answer has %d, numbered from 1)", m[1], len(a.citations))
		}
		used[n] = true
	}
	for n := 1; n <= len(a.citations); n++ {
		if !used[n] {
			return fmt.Errorf("kb_answer response: citation %d is never marked in answer_markdown (write [%d] after what it supports)", n, n)
		}
	}
	return nil
}

// checkKBAnswerResponse holds a kb_answer fixture's response to check. checkResponse has
// already seen an object with one choice that has a message.
func checkKBAnswerResponse(raw json.RawMessage) error {
	tree, err := decodeTree(raw)
	if err != nil {
		return err
	}
	choice := tree.(map[string]any)["choices"].([]any)[0].(map[string]any)
	a, err := readKBAnswer(choice["message"].(map[string]any)["content"])
	if err != nil {
		return err
	}
	return a.check()
}

// quoteError says a citation quotes words that the chunk it cites does not hold.
type quoteError struct {
	n     int
	quote string
}

func (e *quoteError) Error() string {
	return fmt.Sprintf("the quote of citation %d is not in the chunk it cites", e.n)
}

// checkQuotes holds each quote of the (expanded) answer to the chunk of the request its
// chunk_id names. A chunk_id that names no chunk of the request, which a fixture may
// write on purpose, has nothing to be held to.
func (r *resolver) checkQuotes(content any) error {
	a, err := readKBAnswer(content)
	if err != nil {
		return err
	}
	r.parse()
	for i, c := range a.citations {
		for _, chunk := range r.chunks {
			if chunk.id == c.chunkID {
				if !strings.Contains(chunk.text, c.quote) {
					return &quoteError{n: i + 1, quote: c.quote}
				}
				break
			}
		}
	}
	return nil
}
