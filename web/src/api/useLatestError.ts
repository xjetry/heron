import { useRef, useState } from "react";

// 同一时刻只呈现最新一次操作的结果；较早操作的迟到结果被丢弃——它的目标行可能已被后一操作改变，
// 且服务端错误文本自带字段名，用户重试即可再次看到。
export function useLatestError() {
  const latest = useRef(0);
  const [error, setError] = useState<unknown>(null);
  return {
    error,
    // mutationOptions.onMutate 的序号经 mutate 回调的第三个参数传回；成功提示与失败走同一道门，两种结果对称。
    isLatest: (operation: number | undefined) => operation === latest.current,
    // 只有这组回调会展开进 useMutation；展示层方法与第三方配置分层，新增方法不会泄进 MutationCache。
    mutationOptions: {
      onMutate: () => { setError(null); return ++latest.current; },
      onError: (err: unknown, _variables: unknown, operation: number | undefined) => {
        if (operation === latest.current) setError(err);
      },
    },
  };
}
