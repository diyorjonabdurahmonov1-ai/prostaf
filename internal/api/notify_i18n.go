package api

import "fmt"

// What the server says to an account in its own language.
//
// Every message is written once in Uzbek at the call site and looked up here by
// its Uzbek title. A phone set to Russian or English gets the translation; the
// Uzbek text stands when there is none (a message the office typed, a title not
// listed) or when the account is Uzbek. A body of "" means the original body is
// kept — an order's description is the customer's own words, not ours.
//
// A body with verbs (%s, %d) is filled from the args the call site passes
// alongside the Uzbek text, in the same order.

type loc struct{ title, body string }

var notifyI18n = map[string]map[string]loc{
	"Yangi buyurtma!": {
		"ru": {"Новый заказ!", ""},
		"en": {"New order!", ""},
	},
	"Buyurtma qabul qilindi!": {
		"ru": {"Заказ принят!", "%s принял(а) ваш заказ. Телефон: %s"},
		"en": {"Order accepted!", "%s accepted your order. Phone: %s"},
	},
	"Ishchi topildi": {
		"ru": {"Исполнитель найден", "К вашему заказу назначен исполнитель."},
		"en": {"Worker found", "A worker has been assigned to your order."},
	},
	"Sizga buyurtma biriktirildi": {
		"ru": {"Вам назначен заказ", "Администратор назначил вам новый заказ. Подробности — в приложении."},
		"en": {"An order was assigned to you", "The administrator assigned you a new order. See details in the app."},
	},
	"Ishchi yo'lga chiqdi": {
		"ru": {"Исполнитель выехал", "Исполнитель выехал к вам. Следите на карте."},
		"en": {"The worker is on the way", "The worker has set off to you. Follow them on the map."},
	},
	"Ishchi yetib keldi": {
		"ru": {"Исполнитель на месте", "Исполнитель сообщил, что прибыл по вашему адресу."},
		"en": {"The worker has arrived", "The worker says they have arrived at your address."},
	},
	"Ish tugadi": {
		"ru": {"Работа завершена", "Исполнитель сообщил, что закончил работу. Оцените её."},
		"en": {"Job finished", "The worker says the job is done. Please rate it."},
	},
	"Buyurtmangiz rad etildi": {
		"ru": {"Ваш заказ отклонён", "Выбранный исполнитель не принял заказ. Выберите другого или повысьте цену."},
		"en": {"Your order was declined", "The worker you chose did not take the order. Pick another or raise the price."},
	},
	"Buyurtmangizni hech kim qabul qilmadi": {
		"ru": {"Ваш заказ никто не принял", "Поблизости нет свободных исполнителей. Отправьте заказ снова или повысьте цену."},
		"en": {"Nobody took your order", "No free workers nearby. Send the order again or raise the price."},
	},
	"Ishchi buyurtmani bekor qildi": {
		"ru": {"Исполнитель отменил заказ", "Откройте заказ и найдите другого исполнителя."},
		"en": {"The worker cancelled the order", "Open the order to look for another worker."},
	},
	"Buyurtma bekor qilindi": {
		"ru": {"Заказ отменён", ""},
		"en": {"Order cancelled", ""},
	},
	"Buyurtma sizdan olindi": {
		"ru": {"Заказ у вас забрали", "Клиент сообщил, что вы не приехали вовремя. Заказ отправлен другим исполнителям, комиссия не возвращается."},
		"en": {"The order was taken from you", "The customer says you did not arrive on time. The order went to other workers; the fee is not refunded."},
	},
	"Ishga chiqishingiz mumkin": {
		"ru": {"Можно выезжать", "Команда собрана — телефон и адрес клиента открыты."},
		"en": {"You can head out", "The team is complete — the customer's phone and address are now open."},
	},
	"Komissiya qaytarildi": {
		"ru": {"Комиссия возвращена", "Из-за отмены заказа %d сум возвращено на ваш счёт."},
		"en": {"Fee refunded", "Because the order was cancelled, %d so'm was returned to your balance."},
	},
	"Bepul ish qaytarildi": {
		"ru": {"Бесплатный заказ возвращён", "Из-за отмены заказа потраченный бесплатный заказ возвращён."},
		"en": {"Free job returned", "Because the order was cancelled, the free job you used was given back."},
	},
	"Bepul buyurtmalar tugadi": {
		"ru": {"Бесплатные заказы закончились", "Вы использовали бесплатных заказов: %d. Чтобы получать новые заказы, оформите подписку или пополните счёт."},
		"en": {"Free jobs are used up", "You have used %d free jobs. To keep receiving orders, buy a subscription or top up your balance."},
	},
	"Hisobingiz tugadi — oflayn bo'ldingiz": {
		"ru": {"Средства закончились — вы офлайн", "Чтобы получать новые заказы, пополните счёт и снова выйдите онлайн."},
		"en": {"Balance used up — you are offline", "Top up your balance to receive new orders, then go online again."},
	},
	"Bepul buyurtmalar tugadi — oflayn bo'ldingiz": {
		"ru": {"Бесплатные заказы закончились — вы офлайн", "Чтобы получать новые заказы, оформите подписку и снова выйдите онлайн."},
		"en": {"Free jobs are used up — you are offline", "Buy a subscription to receive new orders, then go online again."},
	},
	"Hisobingiz to'ldirildi": {
		"ru": {"Счёт пополнен", "%s сум зачислено на ваш счёт."},
		"en": {"Balance topped up", "%s so'm was added to your balance."},
	},
	"To'lov qabul qilindi": {
		"ru": {"Платёж принят", "Подписка продлена на %d мес. Действует до: %s."},
		"en": {"Payment received", "Your subscription was extended by %d month(s). Valid until: %s."},
	},
	"Obuna faollashtirildi": {
		"ru": {"Подписка активирована", "Ваш аккаунт продлён на %d мес. Действует до: %s."},
		"en": {"Subscription activated", "Your account was extended by %d month(s). Valid until: %s."},
	},
}

// localize returns the title and body for lang. args fill the verbs of the
// translated body; with none given, or an Uzbek account, the originals stand.
func localize(lang, uzTitle, uzBody string, args []any) (string, string) {
	if lang == "" || lang == "uz" {
		return uzTitle, uzBody
	}
	l, ok := notifyI18n[uzTitle][lang]
	if !ok {
		return uzTitle, uzBody
	}
	body := uzBody
	if l.body != "" {
		if len(args) > 0 || !hasVerb(l.body) {
			body = fmt.Sprintf(l.body, args...)
		}
	}
	return l.title, body
}

func hasVerb(s string) bool {
	for i := 0; i+1 < len(s); i++ {
		if s[i] == '%' && s[i+1] != '%' {
			return true
		}
	}
	return false
}
