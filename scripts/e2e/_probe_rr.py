import json, urllib.request
payloads = [
    {"model":"Qwen3-Reranker-8B","query":"health check","documents":["test"]},
    {"model":"Qwen3-Reranker-8B","query":"cat","documents":["a cat"]},
    {"model":"Qwen3-Reranker-8B","query":"health check","documents":["test"],"top_n":1},
]
for p in payloads:
    req = urllib.request.Request("https://ai.gitee.com/v1/rerank", data=json.dumps(p).encode(),
        headers={"Content-Type":"application/json","Authorization":"Bearer QGCT3GIFGLXF934UTEZ6RKZKVJJLV9SEZXN1NL15"}, method="POST")
    try:
        with urllib.request.urlopen(req, timeout=30) as r:
            print(p.get("top_n","-"), "->", r.status, json.loads(r.read().decode()))
    except urllib.error.HTTPError as e:
        print(p.get("top_n","-"), "-> HTTP", e.code, e.read().decode()[:300])
    except Exception as e:
        print("ERR", e)
