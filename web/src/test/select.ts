import { fireEvent, screen } from "@testing-library/react";

// components/Select 的触发按钮名称是「标签 当前值」，选项在点开后的 listbox 里。
export const selectTrigger = (label: string) => screen.getByRole("button", { name: new RegExp(`^${label} `) });
export const querySelectTrigger = (label: string) => screen.queryByRole("button", { name: new RegExp(`^${label} `) });

export function chooseOption(label: string, option: string): void {
  fireEvent.click(selectTrigger(label));
  fireEvent.click(screen.getByRole("option", { name: option }));
}
