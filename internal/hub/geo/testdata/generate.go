//go:build ignore

// 生成自定义测试库，不包含第三方地理数据；固定时间戳使夹具可重现。
package main

import (
	"log"
	"net"
	"os"

	"github.com/maxmind/mmdbwriter"
	"github.com/maxmind/mmdbwriter/mmdbtype"
)

func main() {
	tree, err := mmdbwriter.New(mmdbwriter.Options{DatabaseType: "Probe-Test-Country", Description: map[string]string{"en": "Probe test country fixture"}, BuildEpoch: 1, RecordSize: 24, IncludeReservedNetworks: true})
	if err != nil {
		log.Fatal(err)
	}
	for _, entry := range []struct{ network, code string }{
		{"8.8.8.0/24", "US"}, {"2606:4700::/32", "AU"}, {"10.0.0.0/8", "JP"},
		{"9.9.9.0/24", "us"}, {"11.0.0.0/24", ""},
	} {
		_, network, err := net.ParseCIDR(entry.network)
		if err != nil {
			log.Fatal(err)
		}
		record := mmdbtype.Map{"registered_country": mmdbtype.Map{"iso_code": mmdbtype.String("DE")}}
		if entry.code != "" {
			record["country"] = mmdbtype.Map{"iso_code": mmdbtype.String(entry.code)}
		}
		if err := tree.Insert(network, record); err != nil {
			log.Fatal(err)
		}
	}
	f, err := os.Create("internal/hub/geo/testdata/country.mmdb")
	if err != nil {
		log.Fatal(err)
	}
	if _, err := tree.WriteTo(f); err != nil {
		log.Fatal(err)
	}
	if err := f.Close(); err != nil {
		log.Fatal(err)
	}
}
