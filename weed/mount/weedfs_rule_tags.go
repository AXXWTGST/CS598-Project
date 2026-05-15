package mount

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/seaweedfs/seaweedfs/weed/glog"
	"github.com/seaweedfs/seaweedfs/weed/pb/filer_pb"
	"github.com/seaweedfs/seaweedfs/weed/util"
)

func (wfs *WFS) applyCreateTagRules(path util.FullPath, entry *filer_pb.Entry) []string {
	rules, err := wfs.loadCreateTagRules()
	if err != nil {
		glog.Warningf("load create tag rules %s: %v", wfs.option.RuleJson, err)
		return nil
	}
	if len(rules) == 0 {
		return nil
	}

	ext := strings.ToLower(filepath.Ext(entry.Name))
	tags := rules[ext]
	if len(tags) == 0 {
		return nil
	}

	if entry.Extended == nil {
		entry.Extended = make(map[string][]byte)
	}
	encodedTags := joinTags(tags)
	entry.Extended[SEAWEED_TAGS_KEY] = []byte(encodedTags)
	entry.Extended[SEAWEED_TAG_VER_KEY] = []byte("1")
	glog.V(1).Infof("rule tags on create %s: %s", path, encodedTags)
	return tags
}

func (wfs *WFS) loadCreateTagRules() (map[string][]string, error) {
	if strings.TrimSpace(wfs.option.RuleJson) == "" {
		return nil, nil
	}
	wfs.ruleTagsOnce.Do(func() {
		wfs.ruleTags, wfs.ruleTagsErr = readCreateTagRules(wfs.option.RuleJson)
	})
	return wfs.ruleTags, wfs.ruleTagsErr
}

func readCreateTagRules(path string) (map[string][]string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}

	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		return nil, err
	}

	rules := make(map[string][]string)
	for ext, value := range raw {
		ext = normalizeRuleExt(ext)
		tags, err := parseRuleTags(value)
		if err != nil {
			return nil, err
		}
		tags = normalizeRuleTags(tags)
		if ext != "" && len(tags) > 0 {
			rules[ext] = tags
		}
	}
	return rules, nil
}

func normalizeRuleExt(ext string) string {
	ext = strings.ToLower(strings.TrimSpace(ext))
	if ext == "" {
		return ""
	}
	if !strings.HasPrefix(ext, ".") {
		ext = "." + ext
	}
	return ext
}

func parseRuleTags(value json.RawMessage) ([]string, error) {
	var tags []string
	if err := json.Unmarshal(value, &tags); err == nil {
		return tags, nil
	}
	var tagString string
	if err := json.Unmarshal(value, &tagString); err != nil {
		return nil, err
	}
	return splitTagEventValue(tagString), nil
}

func normalizeRuleTags(tags []string) []string {
	seen := make(map[string]struct{})
	out := make([]string, 0, len(tags))
	for _, tag := range tags {
		tag = strings.TrimSpace(tag)
		if tag == "" {
			continue
		}
		if _, ok := seen[tag]; ok {
			continue
		}
		seen[tag] = struct{}{}
		out = append(out, tag)
	}
	sort.Strings(out)
	return out
}

func joinTags(tags []string) string {
	return strings.Join(tags, ",")
}
