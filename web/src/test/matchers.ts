import { expect } from "vitest";

// vitest 把非对称匹配器的返回值声明成 any，好让它能放进任何位置；可一旦嵌进对象字面量，any 会让外层对象
// 失去类型检查。匹配器对调用方是不透明的值，只用来交给 toEqual、toHaveBeenCalledWith 这类比较，收成 unknown
// 正合它的用途。
export const objectContaining = (expected: object): unknown => expect.objectContaining(expected);
export const stringContaining = (expected: string): unknown => expect.stringContaining(expected);
