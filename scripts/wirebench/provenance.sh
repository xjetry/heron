# shellcheck shell=sh
# 被 run.sh 与 measure-agent.sh source。两者都把"构建与测量用的是哪份源码"记进产物（env.txt / runner-env.txt 的
# commit= 行），run.sh 还把 git describe 当 VERSION 的默认值。
#
# 不变式：记下的提交号描述的是脚本所在的这棵树。git 从当前目录向上找工作区：把发布 archive 解在某个检出里面再跑，
# git 答的是外层检出的 HEAD（go build 盖进二进制的 vcs 信息同样是外层的），记下来就是把别的源码的提交号安在这次
# 测量上。所以：树根就是工作区的根 → 从它的 git 取；树在别的工作区里面 → 拒绝运行；树不在任何工作区里（解到仓库
# 之外的 archive）→ 没有 git 可问，提交号与版本号由调用方经 COMMIT 与 VERSION 给出，缺了同样拒绝。
#
# tree_check <树根>：按上面的规则检查，记下 TREE（物理路径）与 TREE_GIT=yes|no；树在别的工作区里面时失败。
tree_check() {
  TREE=$(cd "$1" && pwd -P)
  if tree_top=$(git -C "$TREE" rev-parse --show-toplevel 2> /dev/null); then
    tree_top=$(cd "$tree_top" && pwd -P)
    if [ "$tree_top" != "$TREE" ]; then
      echo "FAIL: $TREE lies inside the git checkout $tree_top: git (and go build's vcs stamp) would attribute this tree to that checkout's HEAD; extract it outside any checkout and pass COMMIT and VERSION" >&2
      return 1
    fi
    TREE_GIT=yes
  else
    TREE_GIT=no
  fi
}

# tree_commit：输出 "commit=<sha> dirty=yes|no"。没有 git 时输出 "commit=$COMMIT dirty=unknown"，COMMIT 未设即失败。
tree_commit() {
  if [ "$TREE_GIT" = yes ]; then
    if [ -n "$(git -C "$TREE" status --porcelain)" ]; then tree_dirty=yes; else tree_dirty=no; fi
    echo "commit=$(git -C "$TREE" rev-parse HEAD) dirty=$tree_dirty"
  else
    [ -n "${COMMIT:-}" ] || { echo "FAIL: $TREE is not a git checkout; pass COMMIT=<sha of the source it was extracted from>" >&2; return 1; }
    echo "commit=$COMMIT dirty=unknown"
  fi
}

# tree_version：VERSION 的默认值（git describe）。没有 git 时失败，VERSION 必须由调用方给出。
tree_version() {
  if [ "$TREE_GIT" = yes ]; then
    git -C "$TREE" describe --tags --always
  else
    echo "FAIL: $TREE is not a git checkout; pass VERSION" >&2
    return 1
  fi
}
