// Package testdeps 只给测试用。
package testdeps

import (
	"reflect"
	"testing"
)

// RequireEveryField 核对构造函数对依赖结构体逐字段做了非 nil 检查：先用 valid 构造一次（不得 panic），再逐个把字段
// 置零后构造，要求 panic 的值恰为 "<name>.<字段名> must be set"。字段从类型反射枚举，结构体新增的依赖自动进入核对，
// 漏写检查的字段会以 "panic = <nil>" 失败。
func RequireEveryField[D any](t *testing.T, name string, valid D, construct func(D)) {
	t.Helper()
	construct(valid)
	typ := reflect.TypeFor[D]()
	for i := range typ.NumField() {
		field := typ.Field(i).Name
		want := name + "." + field + " must be set"
		deps := valid
		v := reflect.ValueOf(&deps).Elem().Field(i)
		v.Set(reflect.Zero(v.Type()))
		func() {
			defer func() {
				if r := recover(); r != want {
					t.Errorf("%s zeroed: panic = %v, want %s", field, r, want)
				}
			}()
			construct(deps)
		}()
	}
}
