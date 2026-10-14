"""用 botocore 的签名器离线生成签名测试的期望值，被测的 Go 代码不参与。

须用 awscli v2 自带的 Python 运行（它带着 botocore，import awscli 之后才能 import botocore）：

    PY="$(head -1 "$(readlink -f "$(command -v aws)")" | cut -c3-)"
    "$PY" botocore_vectors.py s3 > s3-signing.json
    "$PY" botocore_vectors.py suite get-utf8 get-space get-unreserved

s3：按 hub 的寻址规则（path-style 时 bucket 在路径里、virtual-host 时在主机名里，endpoint 的路径前缀去掉末尾 /
后放在最前）拼出请求 URI，对象键按 botocore 序列化 S3 键的规则编码（percent_encode，保留 / 与 ~），查询串按
botocore 的规范查询串排序，再用 S3SigV4Auth 算 Authorization。
suite：读 <名>/<名>.req，用通用签名器 SigV4Auth（区域 us-east-1、服务 service、AWS 公开的测试凭据）写出
同目录下的 .creq、.sts、.authz。
"""

import hashlib
import json
import os
import sys
from urllib.parse import quote, urlsplit

import awscli  # noqa: F401  让 awscli 自带的 botocore 可以按 botocore 导入
from botocore.auth import S3SigV4Auth, SigV4Auth
from botocore.awsrequest import AWSRequest
from botocore.credentials import Credentials
from botocore.utils import percent_encode

DATE = "20260928T010203Z"
BUCKET = "backups"

ENDPOINTS = [
    {"virtual_host": False, "endpoint": "http://s3.example:9000", "region": "auto"},
    {"virtual_host": False, "endpoint": "http://s3.example/base/", "region": "auto"},
    {"virtual_host": True, "endpoint": "http://s3.example/base", "region": "us-east-1"},
    {"virtual_host": True, "endpoint": "http://s3.example", "region": "auto"},
]

KEYS = [
    "config/2026-09-28T01:02:03Z.db",
    "sp ace/plus+/雪/100%/~tilde",
    "a//b/../c/./d",
    "!*'();:@&=+$,?#[]",
    "a%2Fb",
]

QUERIES = [
    [("list-type", "2"), ("prefix", "a +/雪%"), ("max-keys", "256"), ("encoding-type", "url")],
    [("list-type", "2"), ("prefix", "hub/config/"), ("continuation-token", "1/a+b=c=="), ("encoding-type", "url")],
    [("a-b", "1"), ("a", "2"), ("a", "1")],
]


def s3_vectors():
    out = []
    for e in ENDPOINTS:
        parts = urlsplit(e["endpoint"])
        base = parts.path.rstrip("/")
        host = parts.netloc
        if e["virtual_host"]:
            host = BUCKET + "." + host
        else:
            base += "/" + BUCKET
        signer = S3SigV4Auth(Credentials("access", "secret"), "s3", e["region"])
        cases = [(m, k, [], "payload-" + k if m == "PUT" else "") for k in KEYS for m in ("PUT", "GET", "DELETE")]
        cases += [("GET", "", q, "") for q in QUERIES]
        for method, key, query, payload in cases:
            path = base + "/" + percent_encode(key, safe="/~")
            pairs = sorted((quote(k, safe="-_.~"), quote(v, safe="-_.~")) for k, v in query)
            uri = path + ("?" + "&".join(k + "=" + v for k, v in pairs) if pairs else "")
            digest = hashlib.sha256(payload.encode()).hexdigest()
            req = AWSRequest(method=method, url="http://" + host + uri, headers={"X-Amz-Date": DATE, "X-Amz-Content-SHA256": digest})
            req.context["timestamp"] = DATE
            creq = signer.canonical_request(req)
            sts = signer.string_to_sign(req, creq)
            authz = "AWS4-HMAC-SHA256 Credential=access/%s/%s/s3/aws4_request, SignedHeaders=%s, Signature=%s" % (
                DATE[:8], e["region"], signer.signed_headers(signer.headers_to_sign(req)), signer.signature(sts, req))
            out.append({**e, "method": method, "key": key, "query": query, "payload": payload,
                        "host": host, "request_uri": uri, "canonical_request": creq, "authorization": authz})
    return out


def suite(names):
    signer = SigV4Auth(Credentials("AKIDEXAMPLE", "wJalrXUtnFEMI/K7MDENG+bPxRfiCYEXAMPLEKEY"), "service", "us-east-1")
    root = os.path.dirname(os.path.abspath(__file__))
    for name in names:
        d = os.path.join(root, name)
        head, _, body = open(os.path.join(d, name + ".req"), newline="").read().partition("\n\n")
        lines = head.split("\n")
        method, rest = lines[0].split(" ", 1)
        target = rest.rsplit(" ", 1)[0]
        req = AWSRequest(method=method, url="https://example.amazonaws.com" + target, data=body.encode())
        for line in lines[1:]:
            k, _, v = line.partition(":")
            if k.lower() != "host":
                req.headers[k] = v
        req.context["timestamp"] = req.headers["X-Amz-Date"]
        creq = signer.canonical_request(req)
        sts = signer.string_to_sign(req, creq)
        authz = "AWS4-HMAC-SHA256 Credential=AKIDEXAMPLE/%s/us-east-1/service/aws4_request, SignedHeaders=%s, Signature=%s" % (
            req.context["timestamp"][:8], signer.signed_headers(signer.headers_to_sign(req)), signer.signature(sts, req))
        for ext, text in (("creq", creq), ("sts", sts), ("authz", authz)):
            with open(os.path.join(d, name + "." + ext), "w", newline="") as f:
                f.write(text)


if __name__ == "__main__":
    if sys.argv[1:2] == ["s3"]:
        json.dump(s3_vectors(), sys.stdout, ensure_ascii=False, indent=1)
        sys.stdout.write("\n")
    elif sys.argv[1:2] == ["suite"]:
        suite(sys.argv[2:])
    else:
        sys.exit("usage: botocore_vectors.py s3 | suite <name>...")
