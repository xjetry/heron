import { render } from "@testing-library/react";
import { expect, it } from "vitest";
import { OsIcon, osIconKind } from "./OsIcon";

// agent 上报的 os 是 PRETTY_NAME 或 "macOS <版本>"，用真实样本钉关键词匹配。
it.each([
  ["Debian GNU/Linux 13 (trixie)", "debian"],
  ["Ubuntu 24.04.3 LTS", "ubuntu"],
  ["Alpine Linux v3.21", "alpine"],
  ["Fedora Linux 41 (Server Edition)", "fedora"],
  ["CentOS Stream 9", "centos"],
  ["Rocky Linux 9.5 (Blue Onyx)", "rocky"],
  ["AlmaLinux 9.5 (Teal Serval)", "almalinux"],
  ["Arch Linux", "arch"],
  ["openSUSE Leap 15.6", "opensuse"],
  ["macOS 15.1.1", "macos"],
  // simple-icons 没有 Amazon Linux 图标，与其余未列出的 Linux 一样落通用 Linux 图标。
  ["Amazon Linux 2023.7", "linux"],
  ["Gentoo Linux", "linux"],
  ["FreeBSD 14.1-RELEASE", "host"],
  ["", "host"],
] as const)("osIconKind(%j) 是 %s", (os, kind) => {
  expect(osIconKind(os)).toBe(kind);
});

it("图标是内联 SVG，可访问名与悬停提示都是完整 os 字符串，匹配结果写在 data-os 上", () => {
  const { container, rerender } = render(<OsIcon os="Debian GNU/Linux 13 (trixie)" />);
  const svg = container.querySelector("svg.os-icon");
  expect(svg).toHaveAttribute("role", "img");
  expect(svg).toHaveAttribute("aria-label", "Debian GNU/Linux 13 (trixie)");
  expect(svg).toHaveAttribute("data-os", "debian");
  expect(svg?.querySelector("title")?.textContent).toBe("Debian GNU/Linux 13 (trixie)");
  expect(svg?.querySelector("path")).not.toBeNull();
  // 空 os 没有名字可报，可访问名与悬停提示写“系统未知”。
  rerender(<OsIcon os="" />);
  expect(svg).toHaveAttribute("aria-label", "系统未知");
  expect(svg).toHaveAttribute("data-os", "host");
  expect(svg?.querySelector("title")?.textContent).toBe("系统未知");
});
