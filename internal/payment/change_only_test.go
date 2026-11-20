package payment

import (
	"testing"

	"github.com/stripe/stripe-go/v82"
)

// subItemProration builds a line whose proration Stripe raised on the
// subscription item itself.
func subItemProration() *stripe.InvoiceLineItem {
	return &stripe.InvoiceLineItem{Parent: &stripe.InvoiceLineItemParent{
		Type:                    "subscription_item_details",
		SubscriptionItemDetails: &stripe.InvoiceLineItemParentSubscriptionItemDetails{Proration: true},
	}}
}

// invoiceItemProration builds a line whose proration Stripe raised as a
// pending proration invoice item.
func invoiceItemProration() *stripe.InvoiceLineItem {
	return &stripe.InvoiceLineItem{Parent: &stripe.InvoiceLineItemParent{
		Type:               "invoice_item_details",
		InvoiceItemDetails: &stripe.InvoiceLineItemParentInvoiceItemDetails{Proration: true},
	}}
}

// invoiceItemPlain builds an invoice item line that is not a proration.
func invoiceItemPlain() *stripe.InvoiceLineItem {
	return &stripe.InvoiceLineItem{Parent: &stripe.InvoiceLineItemParent{
		Type:               "invoice_item_details",
		InvoiceItemDetails: &stripe.InvoiceLineItemParentInvoiceItemDetails{Proration: false},
	}}
}

func updateInvoice(lines ...*stripe.InvoiceLineItem) *stripe.Invoice {
	return &stripe.Invoice{
		BillingReason: stripe.InvoiceBillingReasonSubscriptionUpdate,
		Lines:         &stripe.InvoiceLineItemList{Data: lines},
	}
}

func TestChangeOnlyReadsBothProrationShapes(t *testing.T) {
	cases := []struct {
		name string
		inv  *stripe.Invoice
		want bool
	}{
		{"subscription item proration", updateInvoice(subItemProration()), true},
		{"pending proration invoice item", updateInvoice(invoiceItemProration()), true},
		{"both shapes on one invoice", updateInvoice(subItemProration(), invoiceItemProration()), true},
		{"invoice item that is not a proration", updateInvoice(invoiceItemPlain()), false},
		{"a proration beside a non-proration", updateInvoice(subItemProration(), invoiceItemPlain()), false},
	}
	for _, c := range cases {
		if got := changeOnly(c.inv); got != c.want {
			t.Errorf("%s: changeOnly = %v, want %v", c.name, got, c.want)
		}
	}
}
