# 国家库夹具

`country.mmdb` 由本目录的 `generate.go` 自行生成，不含第三方数据库内容。
从仓库根目录重建：

```sh
go get github.com/maxmind/mmdbwriter@v1.2.0
go run internal/hub/geo/testdata/generate.go
go mod tidy
```

读取器为 `github.com/oschwald/maxminddb-golang/v2`；writer 仅用于重建夹具，不是运行依赖。
固定构建时间戳为 Unix 秒 1，IPv6 树同时支持 IPv4。元数据带非空的 description：hub 启动时用读取器的 `Verify` 对库文件做结构校验，它要求这一项非空。

| 网段 | country.iso_code |
|---|---|
| 8.8.8.0/24 | US |
| 2606:4700::/32 | AU |
| 10.0.0.0/8 | JP，证明私网即使库中有值也不会查询 |
| 9.9.9.0/24 | us，证明拒绝非大写码 |
| 11.0.0.0/24 | 缺失，证明不回退到 registered_country |

每条记录均另含 `registered_country.iso_code = DE`，其余地址无记录。
这些国家码是任意测试值，不表示网段的实际位置。
