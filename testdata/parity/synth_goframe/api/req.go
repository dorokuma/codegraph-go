package api

// SignInReq carries the GoFrame reflective route in a g.Meta tag.
// (Fixture text only — not meant to compile against real gf.)
type SignInReq struct {
	g.Meta `path:"/user/sign-in" method:"post" tags:"User"`
	Name   string `json:"name"`
}

// SignInRes response mime tag must NOT become a route.
type SignInRes struct {
	g.Meta `mime:"application/json"`
	Token  string `json:"token"`
}
