package atmos

import (
	"regexp"
	"strings"
)

// Turning Atmos's refusal into a sentence an Uzbek-speaking payer can act on.
//
// # Why this is not just "show what the gateway said"
//
// It was, and the result is a Russian sentence in an Uzbek app: Atmos answers
// with descriptions like "Карта не найдена", and passing those straight through
// puts them in front of somebody who may not read Cyrillic at all.
//
// Worse, the most common one is actively misleading. "Карта не найдена" is
// STPIMS-ERR-004, and to a payer it reads as a verdict on their card — as if
// the bank had lost it. It is almost never that. In practice it means one of:
//
//   - a digit of the card number is wrong;
//   - the expiry is wrong (Atmos's own ERR-090 spells this out: "card not found
//     with this number, the expiry date is incorrect");
//   - the card is genuinely not in the processing this store talks to, which is
//     what a sandbox store says about every real card in the country.
//
// The first two are the payer's to fix and the third is nobody's, so the
// message names the two that are.
//
// # Where the wording comes from
//
// docs.atmos.uz publishes an Uzbek sentence for every STPIMS code, and those
// are used verbatim wherever they are already something a payer can act on.
// They are edited only where the published text is written for an integrator
// rather than a cardholder ("Noto'g'ri hamkor" tells a payer nothing they can
// do), or where — as above — it is true but misleading.
//
// # What is deliberately not translated
//
// A code that is not in this table keeps Atmos's own description. A refusal
// nobody anticipated is better shown in the gateway's words, however awkward,
// than flattened into "something went wrong" — the payer can read it out to
// support, and support can look it up.

// payerMessages maps an Atmos error code to what the person holding the card
// should be told. Keyed on the numeric part, since the same codes appear both
// as "STPIMS-ERR-004" and bare.
var payerMessages = map[string]string{
	// Card not found, in its four spellings. All four say the same thing to a
	// payer, and none of them means what the words suggest.
	"004": "Karta topilmadi. Karta raqami va amal qilish muddatini tekshirib, qaytadan kiriting.",
	"030": "Karta topilmadi. Karta raqami va amal qilish muddatini tekshirib, qaytadan kiriting.",
	"043": "Karta topilmadi. Karta raqami va amal qilish muddatini tekshirib, qaytadan kiriting.",
	"090": "Bu raqamli karta topilmadi — amal qilish muddati noto'g'ri kiritilgan bo'lishi mumkin.",
	"117": "Karta raqami noto'g'ri kiritilgan.",

	// The two that explain a code which never arrives. Both are common here and
	// both are invisible from the app's side: the payment simply stalls on the
	// SMS screen unless the payer is told why.
	"005": "Bu kartaga telefon raqami bog'lanmagan, shuning uchun SMS kod yuborilmaydi. Bankingizga murojaat qiling.",
	"089": "Kartada SMS-xabarnoma xizmati ulanmagan, shuning uchun kod yuborilmaydi. Bankingizga murojaat qiling.",

	// Money.
	"057": "Kartada mablag' yetarli emas.",
	"112": "Kartada to'lov uchun yetarli mablag' yo'q.",

	// Expiry and validity.
	"067": "Kartaning amal qilish muddati tugagan.",
	"076": "Karta yaroqsiz. Bankingizga murojaat qiling.",

	// Blocked, in its several forms. Kept apart rather than merged because
	// "your bank blocked it" and "you blocked it yourself" send the payer to
	// different places.
	"058": "Karta bloklangan. Bankingizga murojaat qiling.",
	"079": "Karta bloklangan (faollashtirilmagan). Bankingizga murojaat qiling.",
	"082": "Karta kredit qarzdorligi sababli bloklangan. Bankingizga murojaat qiling.",
	"087": "Karta vaqtincha bloklangan.",
	"088": "Bu karta egasi tomonidan bloklangan. Bankingizga murojaat qiling.",
	"095": "Foydalanuvchi bloklangan. Bankingizga murojaat qiling.",
	"073": "Karta yo'qolgan deb belgilangan. Bankingizga murojaat qiling.",
	"083": "Virtual karta faollashtirilmagan.",
	"085": "Karta shaxsiylashtirilmagan. Bankingizga murojaat qiling.",

	// The bank refusing, for reasons it does not share.
	"068": "Bank to'lovni rad etdi. Bankingizga murojaat qiling.",
	"069": "Bank kartani shubhali deb belgiladi. Bankingizga murojaat qiling.",
	"070": "Tranzaksiya rad etildi. Bankingizga murojaat qiling.",
	"072": "Bu karta bo'yicha operatsiyalar taqiqlangan. Bankingizga murojaat qiling.",
	"074": "Kartangiz bilan bog'liq muammo bor. Bankingizga murojaat qiling.",
	"075": "Kartangiz bilan bog'liq muammo bor. Bankingizga murojaat qiling.",
	"077": "Kartangiz bilan bog'liq muammo bor. Bankingizga murojaat qiling.",
	"078": "Kartangiz bilan bog'liq muammo bor. Bankingizga murojaat qiling.",
	"086": "Kartangiz bilan bog'liq muammo bor. Bankingizga murojaat qiling.",
	"084": "Bu karta bilan faqat PIN orqali to'lov qilish mumkin.",

	// The code.
	"098": "SMS kod noto'g'ri kiritilgan.",
	"080": "Kod kiritish urinishlari soni oshib ketdi. Bankingizga murojaat qiling.",

	// Try again in a moment.
	"026": "Keyinroq qayta urinib ko'ring.",
	"031": "So'rovlar soni oshib ketdi. Bir oz kutib, qayta urinib ko'ring.",
	"066": "Yaqinda shu hisobga to'lov qildingiz. Bir oz kutib, qayta urinib ko'ring.",
	"120": "To'lov vaqtincha mumkin emas. Keyinroq urinib ko'ring.",
	"024": "Bank bilan aloqa yo'q. Keyinroq qayta urinib ko'ring.",
	"028": "Bank aloqani rad etdi. Keyinroq qayta urinib ko'ring.",
	"041": "Ulanishda xatolik. Keyinroq qayta urinib ko'ring.",
	"094": "To'lov tizimi bilan aloqa yo'q. Keyinroq qayta urinib ko'ring.",

	// The transaction, rather than the card.
	"064": "To'lov bekor qilingan. Yangisini boshlang.",
	"092": "To'lov yopilgan. Yangisini boshlang.",
	"113": "To'lov yopilgan. Yangisini boshlang.",
	"114": "To'lov yopilgan. Yangisini boshlang.",
	"100": "Bu to'lov ilgari bekor qilingan. Yangisini boshlang.",
	"061": "To'lov topilmadi. Yangisini boshlang.",
	"065": "To'lov hali jarayonda. Bir oz kuting.",

	// Amounts.
	"039": "To'lov summasi noto'g'ri.",
	"042": "Bu summani to'lash mumkin emas.",

	// Ours, not theirs. Every one of these means the merchant account or this
	// server is misconfigured, and there is nothing whatever the payer can do
	// about it — so they are told that, plainly, rather than shown a sentence
	// about partners and terminals that reads as their fault.
	"001": operatorMessage,
	"003": operatorMessage,
	"006": operatorMessage,
	"007": operatorMessage,
	"009": operatorMessage,
	"011": operatorMessage,
	"017": operatorMessage,
	"037": operatorMessage,
	"044": operatorMessage,
	"093": operatorMessage,
	"099": operatorMessage,
	"103": operatorMessage,
	"109": operatorMessage,
	"127": operatorMessage,
}

const operatorMessage = "To'lov tizimida sozlama xatosi. Operatorga murojaat qiling."

// stpimsCode matches an error code from the merchant API, and nothing else.
//
// The strictness is the point. MPS reports its own status as a small integer
// with a trace id appended — "5 trace=6602952b…" — and those numbers are not
// STPIMS codes: MPS status 5 has nothing to do with STPIMS-ERR-005 ("card not
// linked to a phone number"). A loose "find the digits" match would read one as
// the other and tell a Visa payer to ring their bank about SMS notifications.
//
// So a code counts only as the documented prefix followed by digits, or as a
// bare three-digit number — which is how the merchant API writes them and how
// MPS never does.
var stpimsCode = regexp.MustCompile(`^STPIMS-ERR-(\d{1,3})$|^(\d{3})$`)

// normalizeCode returns the three-digit key for a merchant-API code, or "" for
// anything that is not one.
func normalizeCode(code string) string {
	m := stpimsCode.FindStringSubmatch(strings.TrimSpace(code))
	if m == nil {
		return ""
	}
	d := m[1]
	if d == "" {
		d = m[2]
	}
	d = strings.TrimLeft(d, "0")
	if d == "" {
		return ""
	}
	for len(d) < 3 {
		d = "0" + d
	}
	return d
}

// payerMessage is what to show for a refusal, or "" to fall back to whatever
// Atmos wrote.
func payerMessage(code string) string {
	if n := normalizeCode(code); n != "" {
		return payerMessages[n]
	}
	return ""
}
