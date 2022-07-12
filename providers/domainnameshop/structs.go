package domainnameshop

type DomainResponse struct {
	ID          int      `json:"id"`
	Domain      string   `json:"domain"`
	Nameservers []string `json:"nameservers"`
}

type DomainNameShopRecord struct {
	ID             int    `json:"id"`
	Host           string `json:"host"`
	TTL            uint16 `json:"ttl,omitempty"`
	Type           string `json:"type"`
	Data           string `json:"data"`
	Priority       string `json:"priority,omitempty"`
	ActualPriority uint16
	Weight         uint16 `json:"weight,omitempty"`
	Port           uint16 `json:"port,omitempty"`
	CAATag         string `json:"tag,omitempty"`
	ActualCAAFlag  string `json:"flags,omitempty"`
	CAAFlag        uint64
	DomainID       string
}
