package api

import (
	"encoding/json"
	"testing"
	"unicode/utf8"
)

func settingsValueGenerators(channelIDs []string) map[string]func() any {
	generators := make(map[string]func() any)
	walkSettings(func(field settingsBudgetField) {
		if field.fd.Message() != nil {
			return
		}
		generators[field.path] = func() any {
			if field.entry.kind == budgetIDList {
				return channelIDs
			}
			_, sample := field.entry.boundary(field.fd)
			return sample
		}
	})
	return generators
}

func boundaryChannelIDs() []string {
	var ids []string
	walkSettings(func(field settingsBudgetField) {
		if field.path == "backup.notify.channel_ids" {
			_, sample := field.entry.boundary(field.fd)
			ids = sample.([]string)
		}
	})
	return ids
}

// 请求覆盖 descriptor 的全部字段，并使用与预算相同的最长字段名；缺少样本即失败。
func settingsBody(t *testing.T, generators map[string]func() any) []byte {
	t.Helper()
	objects := map[string]map[string]any{"": {}}
	walkSettings(func(field settingsBudgetField) {
		if field.fd.Message() != nil {
			object := make(map[string]any)
			objects[field.path] = object
			objects[field.parent][field.name] = object
			return
		}
		generate, ok := generators[field.path]
		if !ok {
			t.Fatalf("missing settings value generator: %s", field.path)
		}
		objects[field.parent][field.name] = generate()
	})
	body, err := json.Marshal(map[string]any{"settings": objects[""]})
	if err != nil {
		t.Fatal(err)
	}
	return body
}

func TestSettingsBudgetTableCoversEveryField(t *testing.T) {
	t.Parallel()
	paths := make(map[string]bool)
	walkSettings(func(field settingsBudgetField) {
		if field.fd.Message() == nil {
			paths[field.path] = true
		}
	})
	generators := settingsValueGenerators(boundaryChannelIDs())
	for path := range paths {
		if _, ok := generators[path]; !ok {
			t.Errorf("settings generators missing leaf %s", path)
		}
	}
	for path := range settingsBudget {
		if !paths[path] {
			t.Errorf("settingsBudget has unknown leaf %s", path)
		}
	}
	for path := range generators {
		if !paths[path] {
			t.Errorf("settings generators have unknown leaf %s", path)
		}
	}
}

func TestSettingsBudgetEntriesAreExact(t *testing.T) {
	t.Parallel()
	walkSettings(func(field settingsBudgetField) {
		if field.fd.Message() != nil {
			return
		}
		t.Run(field.path, func(t *testing.T) {
			// logo 规则从 checkLogo 的接受集推出，只对经它校验的那个字段成立。
			if field.entry.kind == budgetLogo && field.path != "logo" {
				t.Errorf("%s is not validated by checkLogo", field.path)
			}
			want, sample := field.entry.boundary(field.fd)
			values := field.entry.values
			if field.entry.kind == budgetEnumName {
				enums := field.fd.Enum().Values()
				for i := 0; i < enums.Len(); i++ {
					values = append(values, string(enums.Get(i).Name()))
				}
			}
			// 从取值域逐值核对，不依赖 boundary 选择了哪一个样本。
			for _, value := range values {
				if got := jsonStringBytes(value); got > want {
					t.Errorf("%s value %q encodes to %d bytes, above budget %d", field.path, value, got, want)
				}
			}
			if field.path == "accent_color" && !accentRE.MatchString(sample.(string)) {
				t.Errorf("accent_color budget sample %q does not match %s", sample, accentRE)
			}
			encoded, err := json.Marshal(sample)
			if err != nil {
				t.Fatal(err)
			}
			if got := len(encoded); got != want {
				t.Errorf("%s encoded=%d budget=%d difference=%d", field.path, got, want, got-want)
			}
		})
	})
}

func TestSettingsBudgetIsTheWorstBody(t *testing.T) {
	t.Parallel()
	body := settingsBody(t, settingsValueGenerators(boundaryChannelIDs()))
	if len(body) != maxSettingsBody {
		t.Fatalf("boundary body=%d budget=%d difference=%d", len(body), maxSettingsBody, len(body)-maxSettingsBody)
	}
	t.Logf("boundary body=%d budget=%d", len(body), maxSettingsBody)
}

func TestSettingsStringEscapingBound(t *testing.T) {
	t.Parallel()
	for r := rune(0); r <= utf8.MaxRune; r++ {
		if !utf8.ValidRune(r) {
			continue
		}
		encoded, err := json.Marshal(string(r))
		if err != nil {
			t.Fatal(err)
		}
		if got, bound := len(encoded)-2, maxJSONBytesPerUTF8Byte*utf8.RuneLen(r); got > bound {
			t.Fatalf("U+%04X JSON bytes=%d exceeds UTF-8 escape bound=%d", r, got, bound)
		}
	}
}
