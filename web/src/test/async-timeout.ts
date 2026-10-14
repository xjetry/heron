// 页面测试里异步查找的上界。只为挂死时能结束，不参与被测性质。
// vitest 的 testTimeout 必须更长，否则运行器会先杀掉用例，这个上界到不了。
export const asyncUtilTimeout = 30_000;
