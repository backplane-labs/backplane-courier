// courier is a Backplane tool service: it delivers rendered messages —
// mail, push, and whatever a provider plugin adds — through the channels
// of an installation. Products never import it: a binding maps their
// event or hook to courier.SendMail, courier.SendPush, ...
package main

import courier "github.com/backplane-labs/backplane-courier"

func main() { courier.Main() }
