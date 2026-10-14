# SigV4 签名测试向量

期望值都来自独立的签名器，不由被测代码重算。

## 通用套件（`<名>/<名>.req|.creq|.sts|.authz`）

区域 `us-east-1`、服务 `service`、access key `AKIDEXAMPLE`、secret `wJalrXUtnFEMI/K7MDENG+bPxRfiCYEXAMPLEKEY`，
都是 AWS 公开的测试凭据。通用向量不含 S3 的 `x-amz-content-sha256` 头，S3 的请求由下面的 S3 向量另验。

- `get-vanilla`、`post-x-www-form-urlencoded`、`get-header-key-duplicate`、`get-vanilla-query-order-key-case`：
  原样复制自 AWS 维护的 boto/botocore 测试套件，固定版本
  https://github.com/boto/botocore/tree/32302bc372dde1b6173b60f8b85d671e24a0d414/tests/unit/auth/aws4_testsuite ，
  上游许可为 Apache-2.0（`LICENSE.txt`）。
- `get-utf8`、`get-space`、`get-unreserved`：`.req` 按 AWS 通用套件同名用例的请求写出。写入时无法联网取得上游文件，`.creq`、`.sts`、`.authz` 由本机 awscli 2.37.0 自带的 botocore 通用签名器
  `SigV4Auth` 离线算出（`botocore_vectors.py suite …`），尚未与上游文件逐字节核对。同一脚本对上面四组复制来的
  `.req` 重算，得到的 `.creq`、`.sts`、`.authz` 与上游逐字节相同。

S3 与通用 SigV4 的规范 URI 有两处不同：S3 的规范 URI 是对象键编码一次的结果，不规范化路径（不去点段、不合并重复
斜杠）；其余服务先规范化路径，再对请求里（已编码的）路径编码一次，对象名相当于被编码两次。这三组的路径里没有点段、
重复斜杠或 `%`，两种规则得到同一个规范 URI，所以它们对 S3 签名器同样成立（`TestAWSVectors` 用 S3 客户端的
`canonicalRequest` 重算并比对）。通用套件里测路径规范化的其余用例（路径含 `..`、`.` 段或连续斜杠的那些）期望的
正是去点段、合并斜杠之后的规范 URI；对 S3，`a//b/../c` 是一个与 `a/c` 不同的对象键，按规范化之后的路径签名，
服务端按原样的键重算就对不上，所以不收。

## S3 向量（`s3-signing.json`）

由 `botocore_vectors.py s3` 生成：按 hub 的寻址规则拼出请求 URI（path-style 时 bucket 在路径里，virtual-host 时在
主机名里，endpoint 的路径前缀去掉末尾 `/` 后放在最前），对象键按 botocore 序列化 S3 键的规则编码
（`percent_encode`，保留 `/` 与 `~`），查询串按 botocore 的规范查询串排序，再用 awscli 2.37.0 自带 botocore 的
`S3SigV4Auth` 算 Authorization。覆盖 path-style 与 virtual-host、带与不带路径前缀和端口的 endpoint；键含空格、
`+`、中文、`%`、`~`、保留字符、重复斜杠与点段；查询串含重复键与互为前缀的键。`canonical_request` 只用于失败时
对照，不参与断言。

## 重新生成

在本目录下，用 awscli v2 自带的 Python 运行（它带着 botocore）：

```sh
PY="$(head -1 "$(readlink -f "$(command -v aws)")" | cut -c3-)"
"$PY" botocore_vectors.py s3 > s3-signing.json
"$PY" botocore_vectors.py suite get-utf8 get-space get-unreserved
```

换了 awscli 版本就要重跑并在这里改版本号：期望值是对特定版本 botocore 的断言。
