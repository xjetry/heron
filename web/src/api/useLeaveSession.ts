import { useQueryClient } from "@tanstack/react-query";
import { useNavigate } from "react-router";

// 离开会话（Logout、撤销当前会话）时前端缓存必须清空：react-query 的缓存跨会话存活，用户从
// 登录页按回退回到之前的面板页时，那页会直接渲染上一个会话取到的数据——登录页本身不读这些
// 缓存，清空挡住的是回退后的那一页。跳转用 replace 只是不让登录页多占一条历史记录；回退仍会
// 落到之前的面板页，那页重新挂载时照常重新拉取——清不清缓存都会拉，清了只是拉取期间没有旧数据
// 可渲染——随后被 401 挡回登录页。登录页不调用这个 hook。
export function useLeaveSession(): () => void {
  const queryClient = useQueryClient();
  const navigate = useNavigate();
  return () => {
    queryClient.clear();
    void navigate("/login", { replace: true });
  };
}
