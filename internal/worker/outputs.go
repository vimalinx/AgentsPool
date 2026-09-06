package worker

import (
	"fmt"
	"io/fs"
	"mime"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/vimalinx/AgentsPool/internal/core"
)

type outputFile struct {
	AbsolutePath string
	RelativePath string
	MediaType    string
	Size         int64
}

func collectOutputFiles(workspace string, rules []core.OutputRule, maxBytes int64) ([]outputFile, error) {
	if len(rules) == 0 {
		return nil, nil
	}
	type compiledRule struct {
		rule  core.OutputRule
		regex *regexp.Regexp
		count int
	}
	compiled := make([]compiledRule, 0, len(rules))
	for _, rule := range rules {
		regex, err := compileGlob(filepath.ToSlash(filepath.Clean(rule.Pattern)))
		if err != nil {
			return nil, fmt.Errorf("compile output pattern %q: %w", rule.Pattern, err)
		}
		compiled = append(compiled, compiledRule{rule: rule, regex: regex})
	}

	selected := map[string]outputFile{}
	var total int64
	err := filepath.WalkDir(workspace, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if path == workspace {
			return nil
		}
		if entry.Type()&os.ModeSymlink != 0 {
			if entry.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if entry.IsDir() {
			return nil
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		if !info.Mode().IsRegular() {
			return nil
		}
		relative, err := filepath.Rel(workspace, path)
		if err != nil {
			return err
		}
		relative = filepath.ToSlash(relative)
		firstMatchingRule := -1
		for index := range compiled {
			if !compiled[index].regex.MatchString(relative) {
				continue
			}
			compiled[index].count++
			if firstMatchingRule == -1 {
				firstMatchingRule = index
			}
		}
		if firstMatchingRule == -1 {
			return nil
		}
		if _, exists := selected[relative]; exists {
			return nil
		}
		mediaType := compiled[firstMatchingRule].rule.MediaType
		if mediaType == "" {
			mediaType = mime.TypeByExtension(filepath.Ext(relative))
		}
		if mediaType == "" {
			mediaType = "application/octet-stream"
		}
		total += info.Size()
		if maxBytes > 0 && total > maxBytes {
			return fmt.Errorf("matched outputs exceed worker limit of %d bytes", maxBytes)
		}
		selected[relative] = outputFile{AbsolutePath: path, RelativePath: relative, MediaType: mediaType, Size: info.Size()}
		return nil
	})
	if err != nil {
		return nil, err
	}
	for _, rule := range compiled {
		if rule.rule.Required && rule.count == 0 {
			return nil, fmt.Errorf("required output pattern %q matched no files", rule.rule.Pattern)
		}
	}
	out := make([]outputFile, 0, len(selected))
	for _, item := range selected {
		out = append(out, item)
	}
	for i := 1; i < len(out); i++ {
		for j := i; j > 0 && out[j].RelativePath < out[j-1].RelativePath; j-- {
			out[j], out[j-1] = out[j-1], out[j]
		}
	}
	return out, nil
}

func compileGlob(pattern string) (*regexp.Regexp, error) {
	if pattern == "" || pattern == "." || strings.HasPrefix(pattern, "../") || strings.HasPrefix(pattern, "/") {
		return nil, fmt.Errorf("pattern must be relative to the workspace")
	}
	var expression strings.Builder
	expression.WriteString("^")
	for index := 0; index < len(pattern); {
		if pattern[index] == '*' {
			if index+1 < len(pattern) && pattern[index+1] == '*' {
				index += 2
				if index < len(pattern) && pattern[index] == '/' {
					expression.WriteString("(?:.*/)?")
					index++
				} else {
					expression.WriteString(".*")
				}
			} else {
				expression.WriteString("[^/]*")
				index++
			}
			continue
		}
		if pattern[index] == '?' {
			expression.WriteString("[^/]")
			index++
			continue
		}
		expression.WriteString(regexp.QuoteMeta(string(pattern[index])))
		index++
	}
	expression.WriteString("$")
	return regexp.Compile(expression.String())
}
