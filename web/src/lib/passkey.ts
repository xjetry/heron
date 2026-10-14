function decode(value: string): ArrayBuffer {
  const raw = atob(value.replace(/-/g, "+").replace(/_/g, "/"));
  return Uint8Array.from(raw, (c) => c.charCodeAt(0)).buffer;
}

function encode(value: ArrayBuffer): string {
  return btoa(String.fromCharCode(...new Uint8Array(value))).replace(/\+/g, "-").replace(/\//g, "_").replace(/=+$/, "");
}

// hub 的 Begin 应答（go-webauthn 的 CredentialCreation / CredentialAssertion）序列化成 {"publicKey": {...}}：
// 二进制字段（challenge、user.id、凭据 id）是 base64url 字符串，其余字段与浏览器 API 的形状相同，解码后原样交给浏览器。
type DescriptorJSON = Omit<PublicKeyCredentialDescriptor, "id"> & { id: string };
type CreationOptionsJSON = Omit<PublicKeyCredentialCreationOptions, "challenge" | "user" | "excludeCredentials"> & {
  challenge: string;
  user: Omit<PublicKeyCredentialUserEntity, "id"> & { id: string };
  excludeCredentials?: DescriptorJSON[];
};
type RequestOptionsJSON = Omit<PublicKeyCredentialRequestOptions, "challenge" | "allowCredentials"> & {
  challenge: string;
  allowCredentials?: DescriptorJSON[];
};

const descriptors = (list: DescriptorJSON[] | undefined): PublicKeyCredentialDescriptor[] | undefined =>
  list?.map((credential) => ({ ...credential, id: decode(credential.id) }));

function creationOptions(optionsJSON: string): PublicKeyCredentialCreationOptions {
  const { publicKey } = JSON.parse(optionsJSON) as { publicKey: CreationOptionsJSON };
  return { ...publicKey, challenge: decode(publicKey.challenge), user: { ...publicKey.user, id: decode(publicKey.user.id) }, excludeCredentials: descriptors(publicKey.excludeCredentials) };
}

function requestOptions(optionsJSON: string): PublicKeyCredentialRequestOptions {
  const { publicKey } = JSON.parse(optionsJSON) as { publicKey: RequestOptionsJSON };
  return { ...publicKey, challenge: decode(publicKey.challenge), allowCredentials: descriptors(publicKey.allowCredentials) };
}

export async function passkeyCredential(optionsJSON: string, register = false): Promise<string> {
  if (!window.PublicKeyCredential || !navigator.credentials) throw new Error("此浏览器或当前来源不支持 Passkey，请使用 HTTPS。");
  const credential = (register
    ? await navigator.credentials.create({ publicKey: creationOptions(optionsJSON) })
    : await navigator.credentials.get({ publicKey: requestOptions(optionsJSON) })) as PublicKeyCredential | null;
  if (!credential) throw new Error("认证已取消。");
  const response = credential.response;
  const encoded: Record<string, unknown> = { clientDataJSON: encode(response.clientDataJSON) };
  if (response instanceof AuthenticatorAttestationResponse) {
    encoded.attestationObject = encode(response.attestationObject);
    encoded.transports = response.getTransports();
  } else {
    const assertion = response as AuthenticatorAssertionResponse;
    encoded.authenticatorData = encode(assertion.authenticatorData);
    encoded.signature = encode(assertion.signature);
    encoded.userHandle = assertion.userHandle ? encode(assertion.userHandle) : null;
  }
  return JSON.stringify({ id: credential.id, rawId: encode(credential.rawId), type: credential.type, response: encoded, clientExtensionResults: credential.getClientExtensionResults(), authenticatorAttachment: credential.authenticatorAttachment });
}
