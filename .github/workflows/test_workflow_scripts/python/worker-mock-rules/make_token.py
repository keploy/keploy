# make_token.py prints an HS256 JWT signed with jwks.py's key, valid far into the future,
# so a replay long after the record still verifies it.
import base64, hashlib, hmac, json
from jwks import SECRET


def b64e(b):
    return base64.urlsafe_b64encode(b).rstrip(b"=").decode()


h = b64e(json.dumps({"alg": "HS256", "kid": "k1", "typ": "JWT"}).encode())
p = b64e(json.dumps({"sub": "ci", "exp": 4102444800}).encode())
s = b64e(hmac.new(SECRET, f"{h}.{p}".encode(), hashlib.sha256).digest())
print(f"{h}.{p}.{s}")
