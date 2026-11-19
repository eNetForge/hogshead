package policy

import (
	"strings"

	"github.com/enetforge/hogshead/internal/meta"
)

// Caller is the authenticated identity, or nil for anonymous.
type Caller struct {
	User *meta.User
}

// Anonymous reports whether there is no authenticated user.
func (c *Caller) Anonymous() bool { return c == nil || c.User == nil }

// IsAdmin reports whether the caller has the admin role.
func (c *Caller) IsAdmin() bool { return !c.Anonymous() && c.User.Role == meta.RoleAdmin }

// Name returns the user name or "".
func (c *Caller) Name() string {
	if c.Anonymous() {
		return ""
	}
	return c.User.Name
}

// readActions are permitted to the read-only role.
var readActions = map[string]bool{
	"s3:getobject": true, "s3:getobjectversion": true, "s3:listbucket": true, "s3:listbucketversions": true,
	"s3:getbucketlocation": true, "s3:getbucketversioning": true, "s3:getbucketcors": true,
	"s3:getlifecycleconfiguration": true, "s3:getbucketpolicy": true, "s3:listbucketmultipartuploads": true,
	"s3:listmultipartuploadparts": true, "s3:getobjecttagging": true, "s3:getbucketacl": true,
	"s3:getobjectacl": true, "s3:getbucketobjectlockconfiguration": true, "s3:getobjectretention": true,
	"s3:getobjectlegalhold": true, "s3:getbuckettagging": true, "s3:getencryptionconfiguration": true,
	"s3:getobjectversiontagging": true, "s3:getobjectversionacl": true, "s3:getobjectattributes": true,
	"s3:getobjectversionattributes": true,
	"s3:getbucketpublicaccessblock": true, "s3:getbucketlogging": true, "s3:getbucketnotification": true,
	"s3:getbucketrequestpayment": true, "s3:getaccelerateconfiguration": true, "s3:getbucketwebsite": true,
	"s3:getreplicationconfiguration": true, "s3:getbucketownershipcontrols": true,
}

// bucketManagementActions require an unscoped read-write user or admin.
var bucketManagementActions = map[string]bool{
	"s3:createbucket": true,
}

// ownerActions change a bucket's configuration; only the bucket owner or an
// admin may perform them regardless of role.
var ownerActions = map[string]bool{
	"s3:deletebucket": true, "s3:putbucketpolicy": true, "s3:deletebucketpolicy": true, "s3:putbucketacl": true,
	"s3:putbucketcors": true, "s3:putlifecycleconfiguration": true, "s3:putbucketversioning": true,
	"s3:putbuckettagging": true, "s3:putbucketobjectlockconfiguration": true, "s3:putencryptionconfiguration": true,
	"s3:putbucketpublicaccessblock": true, "s3:putbucketownershipcontrols": true, "s3:putbucketnotification": true,
	"s3:bypassgovernanceretention": true,
}

// IsOwnerAction reports whether an action is restricted to the bucket owner.
func IsOwnerAction(action string) bool { return ownerActions[strings.ToLower(action)] }

// Decision explains an authorization result.
type Decision struct {
	Allowed bool
	Reason  string
}

// Authorize decides whether caller may perform r on a bucket with the given
// parsed policy (nil when the bucket has none). bucketOwner is the owning
// user id, or "" when the bucket does not exist yet.
//
// Order: admins may do anything. An explicit policy Deny applies to everyone
// else. Owner-only actions need the owner. Then the caller's role and bucket
// scope are consulted. Finally a policy Allow can grant access, including to
// anonymous callers.
func Authorize(c *Caller, p *Policy, r Request) Decision {
	return AuthorizeOwned(c, p, r, "")
}

// AuthorizeOwned is Authorize with bucket ownership enforced for
// configuration actions.
func AuthorizeOwned(c *Caller, p *Policy, r Request, bucketOwner string) Decision {
	if c.IsAdmin() {
		return Decision{Allowed: true, Reason: "admin"}
	}
	if !c.Anonymous() {
		r.Principal, r.PrincipalID = c.User.Name, c.User.ID
	}
	eff := p.Evaluate(r)
	if eff == Deny {
		return Decision{Reason: "denied by bucket policy"}
	}
	if IsOwnerAction(r.Action) && (c.Anonymous() || bucketOwner == "" || c.User.ID != bucketOwner) {
		return Decision{Reason: "only the bucket owner or an administrator may change bucket configuration"}
	}
	if !c.Anonymous() {
		if c.User.Disabled {
			return Decision{Reason: "user disabled"}
		}
		if roleAllows(c.User, r) {
			return Decision{Allowed: true, Reason: "role"}
		}
	}
	if eff == Allow {
		return Decision{Allowed: true, Reason: "bucket policy"}
	}
	if c.Anonymous() {
		return Decision{Reason: "anonymous access not granted"}
	}
	return Decision{Reason: "not permitted by role or bucket policy"}
}

func roleAllows(u *meta.User, r Request) bool {
	action := strings.ToLower(r.Action)
	if action == "s3:listallmybuckets" {
		return true
	}
	if bucketManagementActions[action] {
		return u.Role == meta.RoleReadWrite && len(u.Buckets) == 0
	}
	if !inScope(u, r.Bucket) {
		return false
	}
	switch u.Role {
	case meta.RoleReadWrite:
		return true
	case meta.RoleReadOnly:
		return readActions[action]
	}
	return false
}

func inScope(u *meta.User, bucket string) bool {
	if len(u.Buckets) == 0 {
		return true
	}
	for _, b := range u.Buckets {
		if b == bucket || glob(b, bucket, false) {
			return true
		}
	}
	return false
}

// VisibleBucket reports whether a user may see bucket in ListBuckets.
func VisibleBucket(c *Caller, bucket string) bool {
	if c.Anonymous() {
		return false
	}
	return c.IsAdmin() || inScope(c.User, bucket)
}
