// 把一段文本存成文件：经 Blob URL 与一次性的 <a download> 触发浏览器下载。
export function downloadText(filename: string, text: string, type = "text/markdown"): void {
  const url = URL.createObjectURL(new Blob([text], { type }));
  const a = document.createElement("a");
  a.href = url;
  a.download = filename;
  a.click();
  // 下载何时读取 URL 由浏览器决定，不在同一轮释放。
  setTimeout(() => URL.revokeObjectURL(url), 60_000);
}
