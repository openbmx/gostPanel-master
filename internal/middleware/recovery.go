package middleware

import (
	stderrors "errors"
	"net/http"
	"runtime/debug"

	"gost-panel/internal/errors"
	"gost-panel/pkg/logger"

	"github.com/gin-gonic/gin"
)

// Recovery 全局异常恢复中间件
func Recovery() gin.HandlerFunc {
	return func(c *gin.Context) {
		defer func() {
			if err := recover(); err != nil {
				// 记录堆栈信息（仅进日志，不外发）
				stack := string(debug.Stack())
				logger.Errorf("系统发生 Panic: path=%s method=%s err=%v\nStack: %s",
					c.Request.URL.Path, c.Request.Method, err, stack)

				// 安全：panic 的值经常包含内部路径、结构体内容甚至凭据片段，
				// 只返回通用文案，细节留在服务端日志里。
				c.JSON(http.StatusInternalServerError, gin.H{
					"code":    errors.ErrInternal.Code,
					"message": errors.ErrInternal.Message,
				})
				c.Abort()
			}
		}()
		c.Next()
	}
}

// ErrorHandler 全局错误处理中间件
// 用于捕获 c.Error() 添加的错误并统一格式化
func ErrorHandler() gin.HandlerFunc {
	return func(c *gin.Context) {
		c.Next()

		//如果没有发生panic，但有 errors
		if len(c.Errors) > 0 {
			// 获取最后一个错误
			err := c.Errors.Last()

			// 此时 response 可能已经写入了，如果没写入则写入 JSON
			if !c.Writer.Written() {
				// 安全：默认使用通用文案，只有显式定义的业务错误才回显其 Message，
				// 避免把内部错误细节（路径、SQL、结构体内容）暴露给客户端。
				status := http.StatusInternalServerError
				resp := gin.H{
					"code":    errors.ErrInternal.Code,
					"message": errors.ErrInternal.Message,
				}

				var bizErr *errors.BizError
				if stderrors.As(err.Err, &bizErr) {
					status = bizErr.HTTPCode
					resp["code"] = bizErr.Code
					resp["message"] = bizErr.Message
				} else {
					logger.Errorf("未处理的请求错误: path=%s err=%v", c.Request.URL.Path, err.Err)
				}

				c.JSON(status, resp)
			}
		}
	}
}
