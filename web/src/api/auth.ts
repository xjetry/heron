import { Code, ConnectError } from "@connectrpc/connect";

// 会话过期、被改密码登出、从未登录，对前端是同一件事：回到登录页。
export function isUnauthenticated(err: unknown): boolean {
  return err instanceof ConnectError && err.code === Code.Unauthenticated;
}

// hub 的错误正文供人阅读，显示层统一去掉 Connect 的错误码前缀。
export function errorText(err: unknown): string {
  return err instanceof ConnectError ? err.rawMessage : String(err);
}
