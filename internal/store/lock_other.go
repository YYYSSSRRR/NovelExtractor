//go:build !unix

package store

// lockDir 在非 unix 平台上不做互斥：这些平台没有 flock 的等价物，
// 而本项目只在 macOS/Linux 上跑。留这个实现是为了让代码本身能跨平台编译，
// 而不是声称在 Windows 上有多实例安全。
func lockDir(dir string) (unlock func(), err error) {
	return func() {}, nil
}
