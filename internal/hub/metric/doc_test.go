package metric

import (
	"os"
	"regexp"
	"slices"
	"strings"
	"testing"
)

// 指标清单写在三处：本包的 Columns（唯一实现）、query.proto 里 MetricSeries.name 与 MetricSample.max 的注释、
// 面板随包下发给主题作者的 heron-theme-skill.md。后两处是给人和 agent 读的说明，加了指标却没改说明，
// 读者就会以为没有这项数据；这里按 Columns 核对它们。

var metricName = regexp.MustCompile(`[a-z][a-z0-9_]*[a-z0-9]`)

// commentBefore 取 proto 里紧挨 field 那一行之前的连续注释。
func commentBefore(t *testing.T, proto, field string) string {
	t.Helper()
	lines := strings.Split(proto, "\n")
	for i, line := range lines {
		if strings.TrimSpace(line) != field {
			continue
		}
		var comment []string
		for j := i - 1; j >= 0 && strings.HasPrefix(strings.TrimSpace(lines[j]), "//"); j-- {
			comment = append([]string{strings.TrimPrefix(strings.TrimSpace(lines[j]), "//")}, comment...)
		}
		return strings.Join(comment, " ")
	}
	t.Fatalf("query.proto 里没有字段 %q", field)
	return ""
}

func columnNames(kinds ...Kind) []string {
	var names []string
	for _, c := range Columns {
		if len(kinds) == 0 || slices.Contains(kinds, c.Kind) {
			names = append(names, c.Name)
		}
	}
	return names
}

func TestQueryProtoCommentsListEveryMetric(t *testing.T) {
	raw, err := os.ReadFile("../../../proto/heron/v1/query.proto")
	if err != nil {
		t.Fatal(err)
	}
	proto := string(raw)
	// 注释的清单在第一个句号之前；句号之后的说明里出现的英文词（如 metric.Columns 的路径）不算指标名。
	list := func(field string) []string {
		comment, _, _ := strings.Cut(commentBefore(t, proto, field), "。")
		return metricName.FindAllString(comment, -1)
	}
	if got, want := list("string name = 1;"), columnNames(); !slices.Equal(got, want) {
		t.Errorf("MetricSeries.name 注释的指标清单 = %v，Columns = %v", got, want)
	}
	if got, want := list("optional double max = 3;"), columnNames(MeanMax); !slices.Equal(got, want) {
		t.Errorf("MetricSample.max 注释列出的带峰值指标 = %v，Columns 里 MeanMax 的是 %v", got, want)
	}
}

func TestThemeSkillMetricTableMatchesColumns(t *testing.T) {
	raw, err := os.ReadFile("../../../web/src/assets/heron-theme-skill.md")
	if err != nil {
		t.Fatal(err)
	}
	row := regexp.MustCompile("(?m)^\\| `([a-z0-9_]+)` \\| ([^|]+) \\| ([^|]+) \\|$")
	values := map[Kind]string{Mean: "mean", MeanMax: "mean、max", Sum: "sum"}
	var got, want []string
	for _, m := range row.FindAllStringSubmatch(string(raw), -1) {
		got = append(got, m[1]+" "+strings.TrimSpace(m[2])+" "+strings.TrimSpace(m[3]))
	}
	for _, c := range Columns {
		unit := c.Unit
		if unit == "" {
			unit = "（空）"
		}
		want = append(want, c.Name+" "+unit+" "+values[c.Kind])
	}
	if !slices.Equal(got, want) {
		t.Errorf("heron-theme-skill.md 的指标表\n得到 %q\n应为 %q", got, want)
	}
}
