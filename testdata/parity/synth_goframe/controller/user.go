package controller

import "context"

// Controller serves GoFrame-bound routes reflectively.
type Controller struct{}

// SignIn joins to POST /user/sign-in via *api.SignInReq in the signature.
func (c *Controller) SignIn(ctx context.Context, req *api.SignInReq) error {
	if req == nil {
		return nil
	}
	return finishSignIn(req.Name)
}

func finishSignIn(name string) error {
	_ = name
	return nil
}
