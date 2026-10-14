package api

import (
	"encoding/json"
	"fmt"
	"math"
	"regexp/syntax"
	"strconv"
	"strings"
	"unicode/utf8"

	heronv1 "github.com/xjetry/heron/gen/heron/v1"
	"google.golang.org/protobuf/reflect/protoreflect"
)

type budgetKind uint8

const (
	// 任意文本按 UTF-8 字节上限取界；每字节最多膨胀六倍，另计引号。
	budgetEscapedString budgetKind = iota
	// logo 的上界从 checkLogo 的接受集推出：前缀集取 JSON 编码最长的前缀，其余字节按字母表里膨胀最大的字符计。
	budgetLogo
	// 有限字符串集合取编码最长者；bool 没有字符串集合，取较长的 false。
	budgetLiteral
	// protojson 用枚举名编码，取 descriptor 中最长的名字。
	budgetEnumName
	// 数值没有业务字节上限常量；uint32 类型上限为十位，比当前业务上限多几字节，
	// 按类型宽度取界使登记项不依赖 store 的业务范围表。
	budgetUint32
	// 渠道 ID 的合法域是正 int64；负数与冗余数字写法不在预算内。
	// 与 uint32 按类型上限不同，这里由数据库从 1 开始分配的 ID 域取界。
	budgetIDList
)

const maxJSONBytesPerUTF8Byte = 6

// 表只登记类型与参数，字节上界与达到上界的样本都由类型规则产生。
// 参数按字节上限、条数或有限取值集合登记；边界样本不要求通过业务校验。
type budgetEntry struct {
	kind   budgetKind
	limit  int
	values []string
}

var settingsBudget = map[string]budgetEntry{
	"title":                      {kind: budgetEscapedString, limit: maxTitleBytes},
	"theme":                      {kind: budgetLiteral, values: themes},
	"accent_color":               {kind: budgetLiteral, values: []string{regexpBudgetSample(accentRE.String())}},
	"logo":                       {kind: budgetLogo, limit: maxLogoBytes},
	"custom_css":                 {kind: budgetEscapedString, limit: maxCSSBytes},
	"public_enabled":             {kind: budgetLiteral},
	"geo_enabled":                {kind: budgetLiteral},
	"geo_url":                    {kind: budgetEscapedString, limit: maxGeoURLBytes},
	"geo_backend":                {kind: budgetEnumName},
	"geo_mmdb_path":              {kind: budgetEscapedString, limit: maxMMDBPathBytes},
	"backup.endpoint":            {kind: budgetEscapedString, limit: maxEndpointBytes},
	"backup.bucket":              {kind: budgetEscapedString, limit: maxBucketBytes},
	"backup.region":              {kind: budgetEscapedString, limit: maxRegionBytes},
	"backup.access_key":          {kind: budgetEscapedString, limit: maxAccessKeyBytes},
	"backup.secret":              {kind: budgetEscapedString, limit: maxSecretBytes},
	"backup.prefix":              {kind: budgetEscapedString, limit: maxPrefixBytes},
	"backup.config_interval_s":   {kind: budgetUint32},
	"backup.metrics_interval_s":  {kind: budgetUint32},
	"backup.config_keep":         {kind: budgetUint32},
	"backup.metrics_keep":        {kind: budgetUint32},
	"backup.notify.channel_ids":  {kind: budgetIDList, limit: maxNotifyChannels},
	"backup.has_secret":          {kind: budgetLiteral},
	"login_notify.channel_ids":   {kind: budgetIDList, limit: maxNotifyChannels},
	"heartbeat.url":              {kind: budgetEscapedString, limit: maxHeartbeatURLBytes},
	"heartbeat.interval_s":       {kind: budgetUint32},
	"heartbeat.method":           {kind: budgetEnumName},
	"heartbeat.has_url":          {kind: budgetLiteral},
	"heartbeat.url_host":         {kind: budgetEscapedString, limit: maxHeartbeatURLBytes},
	"traffic_report.enabled":     {kind: budgetLiteral},
	"traffic_report.daily":       {kind: budgetLiteral},
	"traffic_report.weekly":      {kind: budgetLiteral},
	"traffic_report.monthly":     {kind: budgetLiteral},
	"traffic_report.hour":        {kind: budgetUint32},
	"traffic_report.channel_ids": {kind: budgetIDList, limit: maxNotifyChannels},
}

func jsonStringBytes(value string) int {
	b, err := json.Marshal(value)
	if err != nil {
		panic(err)
	}
	return len(b)
}

func longestJSONValue(values []string) string {
	if len(values) == 0 {
		panic("empty settings budget literal set")
	}
	longest := values[0]
	for _, value := range values[1:] {
		if jsonStringBytes(value) > jsonStringBytes(longest) {
			longest = value
		}
	}
	return longest
}

func (e budgetEntry) boundary(fd protoreflect.FieldDescriptor) (int, any) {
	if e.limit < 0 || fd.IsList() != (e.kind == budgetIDList) {
		panic(fmt.Sprintf("invalid settings budget rule for %s", fd.FullName()))
	}
	switch {
	case e.kind == budgetEscapedString && fd.Kind() == protoreflect.StringKind:
		return maxJSONBytesPerUTF8Byte*e.limit + 2, strings.Repeat("\x01", e.limit)
	case e.kind == budgetLogo && fd.Kind() == protoreflect.StringKind:
		return logoBoundary(e.limit)
	case e.kind == budgetLiteral && fd.Kind() == protoreflect.BoolKind:
		return len("false"), false
	case e.kind == budgetLiteral && fd.Kind() == protoreflect.StringKind:
		value := longestJSONValue(e.values)
		return jsonStringBytes(value), value
	case e.kind == budgetEnumName && fd.Kind() == protoreflect.EnumKind:
		values := fd.Enum().Values()
		names := make([]string, values.Len())
		for i := range names {
			names[i] = string(values.Get(i).Name())
		}
		value := longestJSONValue(names)
		return jsonStringBytes(value), value
	case e.kind == budgetUint32 && fd.Kind() == protoreflect.Uint32Kind:
		return len(strconv.FormatUint(math.MaxUint32, 10)), uint32(math.MaxUint32)
	case e.kind == budgetIDList && fd.Kind() == protoreflect.Int64Kind:
		ids := make([]string, e.limit)
		for i := range ids {
			ids[i] = strconv.FormatInt(math.MaxInt64-int64(i), 10)
		}
		if e.limit == 0 {
			return len(`[]`), ids
		}
		return e.limit*maxChannelIDJSONBytes + 2 - 1, ids
	default:
		panic(fmt.Sprintf("settings budget kind %d does not model %s", e.kind, fd.FullName()))
	}
}

// logoBoundary 从 checkLogo 的接受集（logoTypes 的前缀 × isBase64Char 的字母表）推 logo 的上界与样本：
// 前缀取 JSON 编码后最长的一个，余下的字节全部填字母表里 JSON 膨胀最大的字符。字母表或前缀集放宽时上界
// 随之变大，登记项不用改；字母表只建模单字节字符，出现多字节字符必须先扩展模型。
func logoBoundary(limit int) (int, any) {
	worstChar, factor := logoAlphabetWorst()
	best, sample := -1, ""
	for _, mediaType := range logoTypes {
		prefix := logoPrefix(mediaType)
		if len(prefix) > limit {
			panic("settings budget: logo prefix " + prefix + " exceeds maxLogoBytes")
		}
		if n := jsonStringBytes(prefix) + (limit-len(prefix))*factor; n > best {
			best, sample = n, prefix+strings.Repeat(worstChar, limit-len(prefix))
		}
	}
	return best, sample
}

// logoAlphabetWorst 在 isBase64Char 接受的全部字符里找 JSON 编码最长的那个及其每字节膨胀倍数。
func logoAlphabetWorst() (string, int) {
	worst, factor := "", 0
	for r := rune(0); r <= utf8.MaxRune; r++ {
		if !isBase64Char(r) {
			continue
		}
		if utf8.RuneLen(r) != 1 {
			panic(fmt.Sprintf("settings budget: logo alphabet accepts multi-byte U+%04X; extend budget model", r))
		}
		if n := jsonStringBytes(string(r)) - 2; n > factor {
			worst, factor = string(r), n
		}
	}
	if factor == 0 {
		panic("settings budget: logo alphabet is empty")
	}
	return worst, factor
}

// 主色样本从校验正则的有限语言取界，不能把固定六位样本与 accentRE 分开维护。
// 无界重复等未建模的语法必须先扩展预算模型，不能悄悄截取一个较短样本。
func regexpBudgetSample(pattern string) string {
	re, err := syntax.Parse(pattern, syntax.Perl)
	if err != nil {
		panic(err)
	}
	var sample func(*syntax.Regexp) string
	sample = func(re *syntax.Regexp) string {
		switch re.Op {
		case syntax.OpBeginText, syntax.OpEndText, syntax.OpEmptyMatch:
			return ""
		case syntax.OpLiteral:
			if re.Flags&syntax.FoldCase != 0 {
				panic("case-folded settings literal needs a budget model")
			}
			return string(re.Rune)
		case syntax.OpCharClass:
			var values []string
			for i := 0; i < len(re.Rune); i += 2 {
				for r := re.Rune[i]; r <= re.Rune[i+1]; r++ {
					values = append(values, string(r))
				}
			}
			return longestJSONValue(values)
		case syntax.OpCapture, syntax.OpQuest:
			return sample(re.Sub[0])
		case syntax.OpRepeat:
			if re.Max < 0 {
				panic("unbounded settings literal needs a budget model")
			}
			return strings.Repeat(sample(re.Sub[0]), re.Max)
		case syntax.OpConcat:
			var out strings.Builder
			for _, sub := range re.Sub {
				out.WriteString(sample(sub))
			}
			return out.String()
		case syntax.OpAlternate:
			var values []string
			for _, sub := range re.Sub {
				values = append(values, sample(sub))
			}
			return longestJSONValue(values)
		default:
			panic(fmt.Sprintf("settings literal regexp %s needs a budget model", re.Op))
		}
	}
	return sample(re)
}

type settingsBudgetField struct {
	path, parent, name string
	first              bool
	fd                 protoreflect.FieldDescriptor
	entry              budgetEntry
}

// 预算、覆盖检查和请求样本共用 descriptor 遍历。未建模形状必须在包初始化计算预算时失败。
// 名字取 protojson 接受的原名与 JSON 名中编码较长者；样本也使用这一拼写。
func walkSettings(fn func(settingsBudgetField)) {
	root := (&heronv1.Settings{}).ProtoReflect().Descriptor()
	var walk func(protoreflect.MessageDescriptor, string)
	walk = func(md protoreflect.MessageDescriptor, parent string) {
		for i := 0; i < md.Fields().Len(); i++ {
			fd := md.Fields().Get(i)
			path := string(fd.Name())
			if parent != "" {
				path = parent + "." + path
			}
			oneof := fd.ContainingOneof()
			if fd.IsMap() || (fd.Message() != nil && (fd.IsList() || fd.Message().ParentFile().Package() != root.ParentFile().Package())) ||
				(oneof != nil && !oneof.IsSynthetic()) {
				panic("settings budget: " + path + ": extend budget model before adding this field shape")
			}
			name := string(fd.Name())
			if jsonStringBytes(fd.JSONName()) > jsonStringBytes(name) {
				name = fd.JSONName()
			}
			field := settingsBudgetField{path: path, parent: parent, name: name, first: i == 0, fd: fd}
			if fd.Message() == nil {
				var ok bool
				field.entry, ok = settingsBudget[path]
				if !ok {
					panic("settingsBudget missing leaf " + path)
				}
			}
			fn(field)
			if fd.Message() != nil {
				walk(fd.Message(), path)
			}
		}
	}
	walk(root, "")
}

// 每个对象只在字段之间计逗号；字段名使用 JSON 编码长度，大括号与请求骨架各计一次。
func budgetTotal() int {
	total := len(`{"settings":{}}`)
	walkSettings(func(field settingsBudgetField) {
		total += jsonStringBytes(field.name) + len(`:`)
		if !field.first {
			total++
		}
		if field.fd.Message() != nil {
			total += len(`{}`)
		} else {
			n, _ := field.entry.boundary(field.fd)
			total += n
		}
	})
	return total
}

var maxSettingsBody = budgetTotal()
