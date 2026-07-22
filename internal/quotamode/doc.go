// Package quotamode implements owner-selectable quota policy modes, short-lived
// soft reservations against normalized provider windows, and post-attempt usage
// attribution (V090-099).
//
// Soft reservations prevent two concurrent candidates from treating the same
// unreserved remaining capacity as fully available. Policy modes never
// substitute an explicit pin. Local usage attribution records drift without
// fabricating provider-authoritative remaining values.
package quotamode
