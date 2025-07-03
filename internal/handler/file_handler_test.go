package handler

import (
	"fmt"
	"net/url"
	"testing"
)

func TestQueryUnescape(t *testing.T) {
	str := "affordance/images/rtx_frames_success_0/10_utokyo_pr2_tabletop_manipulation_converted_externally_to_rlds#episode_106"
	// filePath := url.QueryEscape(str) // 该方法会将/转成%2F，不能使用。
	// fmt.Println(filePath)

	// u, err := url.Parse(str) // 没有效果，还是存在#号
	// if err != nil {
	// 	// 处理错误
	// }
	// filePath1 := u.String()
	// fmt.Println(filePath1)

	filePath1 := url.PathEscape(str) // 和第1个有一样的问题
	fmt.Println(filePath1)

	filePath, err := url.QueryUnescape(filePath1)
	if err != nil {
		fmt.Println(err)
	}
	fmt.Println(filePath)
}
