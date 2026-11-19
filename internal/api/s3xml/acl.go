package s3xml

import "encoding/xml"

// Grantee xsi:type values.
const (
	GranteeCanonicalUser         = "CanonicalUser"
	GranteeGroup                 = "Group"
	GranteeAmazonCustomerByEmail = "AmazonCustomerByEmail"
)

// ACL permission values.
const (
	PermissionFullControl = "FULL_CONTROL"
	PermissionRead        = "READ"
	PermissionWrite       = "WRITE"
	PermissionReadACP     = "READ_ACP"
	PermissionWriteACP    = "WRITE_ACP"
)

// Grantee is the recipient of a Grant. It renders as
//
//	<Grantee xmlns:xsi="http://www.w3.org/2001/XMLSchema-instance" xsi:type="CanonicalUser">
//
// which is what encoding/xml produces from the literal "xmlns:xsi" and
// "xsi:type" attribute tags. On decode, encoding/xml resolves the prefix to a
// namespace and no longer matches the literal tag, so UnmarshalXML copies
// the type attribute by local name.
type Grantee struct {
	XMLName      xml.Name `xml:"Grantee"`
	XmlnsXsi     string   `xml:"xmlns:xsi,attr,omitempty"`
	Type         string   `xml:"xsi:type,attr,omitempty"`
	ID           string   `xml:"ID,omitempty"`
	DisplayName  string   `xml:"DisplayName,omitempty"`
	URI          string   `xml:"URI,omitempty"`
	EmailAddress string   `xml:"EmailAddress,omitempty"`
}

// UnmarshalXML decodes a Grantee and recovers xsi:type regardless of how
// the prefix was declared.
func (g *Grantee) UnmarshalXML(d *xml.Decoder, start xml.StartElement) error {
	type plain Grantee
	var p plain
	if err := d.DecodeElement(&p, &start); err != nil {
		return err
	}
	for _, a := range start.Attr {
		if a.Name.Local == "type" && (a.Name.Space == XsiNS || a.Name.Space == "xsi" || a.Name.Space == "") {
			p.Type = a.Value
		}
	}
	p.XmlnsXsi = XsiNS
	*g = Grantee(p)
	return nil
}

// Grant pairs a Grantee with a Permission.
type Grant struct {
	Grantee    Grantee `xml:"Grantee"`
	Permission string  `xml:"Permission"`
}

// AccessControlPolicy is the Get/PutBucketAcl and Get/PutObjectAcl body.
type AccessControlPolicy struct {
	XMLName           xml.Name `xml:"AccessControlPolicy"`
	Xmlns             string   `xml:"xmlns,attr,omitempty"`
	Owner             Owner    `xml:"Owner"`
	AccessControlList []Grant  `xml:"AccessControlList>Grant"`
}

// FullControlACL returns the canonical "owner has FULL_CONTROL" document.
// hogshead does not implement real ACLs, so every GetAcl renders this.
func FullControlACL(owner Owner) AccessControlPolicy {
	return AccessControlPolicy{
		Owner: owner,
		AccessControlList: []Grant{{
			Grantee: Grantee{
				XmlnsXsi:    XsiNS,
				Type:        GranteeCanonicalUser,
				ID:          owner.ID,
				DisplayName: owner.DisplayName,
			},
			Permission: PermissionFullControl,
		}},
	}
}
