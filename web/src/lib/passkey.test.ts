import { afterEach, expect, it, vi } from "vitest";
import { passkeyCredential } from "./passkey";

afterEach(() => { vi.unstubAllGlobals(); });

const bytes = (buffer: BufferSource | undefined) => [...new Uint8Array(buffer as ArrayBuffer)];
// base64url 的 "-_" 与无填充都要解：0xfb 0xff 0xbf 编成 "-_-_"。
const b64url = "-_-_";
const raw = [0xfb, 0xff, 0xbf];

class AttestationResponse {
  clientDataJSON = new Uint8Array([1]).buffer;
  attestationObject = new Uint8Array([2]).buffer;
  getTransports() { return ["internal"]; }
}
class AssertionResponse {
  clientDataJSON = new Uint8Array([1]).buffer;
  authenticatorData = new Uint8Array([3]).buffer;
  signature = new Uint8Array([4]).buffer;
  userHandle = null;
}

function stubCredentials(response: object) {
  const credential = { id: "cred", rawId: new Uint8Array([9]).buffer, type: "public-key", response, getClientExtensionResults: () => ({}), authenticatorAttachment: "platform" };
  const create = vi.fn<(options: CredentialCreationOptions) => Promise<unknown>>(async () => credential);
  const get = vi.fn<(options: CredentialRequestOptions) => Promise<unknown>>(async () => credential);
  vi.stubGlobal("PublicKeyCredential", class {});
  vi.stubGlobal("AuthenticatorAttestationResponse", AttestationResponse);
  vi.stubGlobal("navigator", { credentials: { create, get } });
  return { create, get };
}

// hub 下发的二进制字段是 base64url 字符串，交给浏览器前解成字节；其余字段原样透传。
it("注册：challenge、user.id 与排除的凭据 id 解成字节，其余字段原样交给浏览器", async () => {
  const { create } = stubCredentials(new AttestationResponse());
  const publicKey = {
    rp: { id: "hub.example", name: "Heron" },
    user: { id: b64url, name: "admin", displayName: "admin" },
    challenge: b64url,
    pubKeyCredParams: [{ type: "public-key", alg: -7 }],
    timeout: 60000,
    excludeCredentials: [{ id: b64url, type: "public-key", transports: ["internal"] }],
    authenticatorSelection: { residentKey: "required" },
  };
  const out = JSON.parse(await passkeyCredential(JSON.stringify({ publicKey }), true)) as { response: Record<string, unknown> };
  const passed = create.mock.calls[0][0].publicKey!;
  expect(bytes(passed.challenge)).toEqual(raw);
  expect(bytes(passed.user.id)).toEqual(raw);
  expect(passed.excludeCredentials?.map((c) => bytes(c.id))).toEqual([raw]);
  expect(passed.excludeCredentials?.[0].transports).toEqual(["internal"]);
  expect(passed).toMatchObject({ rp: publicKey.rp, pubKeyCredParams: publicKey.pubKeyCredParams, timeout: 60000, authenticatorSelection: { residentKey: "required" }, user: { name: "admin", displayName: "admin" } });
  expect(out.response).toEqual({ clientDataJSON: "AQ", attestationObject: "Ag", transports: ["internal"] });
});

it("登录：challenge 与允许的凭据 id 解成字节，走 get 而不是 create", async () => {
  const { create, get } = stubCredentials(new AssertionResponse());
  const publicKey = { challenge: b64url, rpId: "hub.example", userVerification: "preferred", allowCredentials: [{ id: b64url, type: "public-key" }] };
  const out = JSON.parse(await passkeyCredential(JSON.stringify({ publicKey }))) as { response: Record<string, unknown> };
  expect(create).not.toHaveBeenCalled();
  const passed = get.mock.calls[0][0].publicKey!;
  expect(bytes(passed.challenge)).toEqual(raw);
  expect(passed.allowCredentials?.map((c) => bytes(c.id))).toEqual([raw]);
  expect(passed).toMatchObject({ rpId: "hub.example", userVerification: "preferred" });
  expect(out.response).toEqual({ clientDataJSON: "AQ", authenticatorData: "Aw", signature: "BA", userHandle: null });
});
