package api

import (
	"context"
	"io/fs"

	"connectrpc.com/connect"

	heronv1 "github.com/xjetry/heron/gen/heron/v1"
	protosrc "github.com/xjetry/heron/proto"
)

// GetApiReference 下发构建时嵌入的技能文件与 proto 源文件。WalkDir 按字典序遍历，
// 响应顺序因而稳定；嵌入的文件系统只读且随二进制固定，读失败只可能是构建缺陷。
func (s *Service) GetApiReference(ctx context.Context, _ *connect.Request[heronv1.GetApiReferenceRequest]) (*connect.Response[heronv1.GetApiReferenceResponse], error) {
	out := &heronv1.GetApiReferenceResponse{Guide: protosrc.Guide}
	err := fs.WalkDir(protosrc.Files, ".", func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		b, err := fs.ReadFile(protosrc.Files, path)
		if err != nil {
			return err
		}
		out.Files = append(out.Files, &heronv1.ProtoFile{Path: path, Content: string(b)})
		return nil
	})
	if err != nil {
		s.log.Error("reading embedded proto sources failed", "err", err)
		return nil, internalError("reading embedded proto sources failed")
	}
	return connect.NewResponse(out), nil
}
