// Excel OAuth protocol adapted from ranxi2001/sub2api (e90811e), LGPL-3.0.
package openai

import "net/url"

const ExcelClientID = "app_fnr0pYvVwwFDocDumLG3H2Bp"
const ExcelRedirectURI = "https://bps.openai.com/basispoints/extension/360590d7-f8f9-4d88-bf75-0edfe0a4b9f3/auth/callback"
const ExcelTokenURL = "https://auth.openai.com/oauth/token?unified=true"

func BuildExcelAuthorizationURL(state, challenge string) string {
	params := url.Values{
		"client_id": {ExcelClientID}, "response_type": {"code"},
		"redirect_uri": {ExcelRedirectURI}, "scope": {"openid offline_access email profile organization.read"},
		"state": {state}, "code_challenge_method": {"S256"}, "code_challenge": {challenge},
		"audience": {"https://api.openai.com/v1"}, "platform": {"PC"},
	}
	return "https://auth.openai.com/api/accounts/authorize?" + params.Encode()
}
