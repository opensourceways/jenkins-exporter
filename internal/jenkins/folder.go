package jenkins

import (
	"context"
	"fmt"
)

// EnumerateLeafJobs 从指定路径（空串为顶层）递归下钻 folder，返回全部叶子 job 的全路径。
// 仅返回非 folder 的叶子 job；folder 类型按 /job/<seg> 逐层下钻。
// path 已是相对于 base 的全路径（如 "folder/sub"），递归时拼接子 job name。
func EnumerateLeafJobs(ctx context.Context, ds DataSource, path string) ([]string, error) {
	jobs, err := ds.Jobs(ctx, path)
	if err != nil {
		return nil, fmt.Errorf("拉取 job 列表失败 path=%q: %w", path, err)
	}
	var leaves []string
	for _, j := range jobs {
		full := j.Name
		if path != "" {
			full = path + "/" + j.Name
		}
		if IsFolderClass(j.Class) {
			sub, err := EnumerateLeafJobs(ctx, ds, full)
			if err != nil {
				return nil, err
			}
			leaves = append(leaves, sub...)
			continue
		}
		leaves = append(leaves, full)
	}
	return leaves, nil
}
