package api

import "net/http"

// GET /privacy — the privacy policy: what is collected, why, who else sees it,
// how long it is kept, and how to have it erased. Public, in Uzbek, Russian and
// English, and outside /api so it is an ordinary web page. Both app stores ask
// for its address, and the public offer (section 16) refers to it.
//
// It describes what the code does, and must be changed with the code: a new kind
// of data, a new outside service, a different retention rule — each is a line
// here. See also /delete-account.
func (s *Server) handlePrivacyPage(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "public, max-age=3600")
	_, _ = w.Write([]byte(privacyHTML))
}

const privacyHTML = `<!doctype html>
<html lang="uz">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>PROSTAFF — Maxfiylik siyosati</title>
<style>
  body{font:16px/1.6 system-ui,-apple-system,Segoe UI,Roboto,sans-serif;margin:0;background:#f6f7f9;color:#14181f}
  main{max-width:760px;margin:0 auto;padding:28px 20px 70px}
  h1{font-size:26px;margin:0 0 4px} h2{font-size:18px;margin:26px 0 8px}
  section{background:#fff;border-radius:14px;padding:18px 22px;margin:16px 0;box-shadow:0 1px 3px rgba(0,0,0,.06)}
  ul{padding-left:22px;margin:6px 0} li{margin:5px 0} .meta{color:#5b6472;font-size:14px}
  a{color:#1b6ef3}
</style>
</head>
<body><main>
<h1>PROSTAFF — Maxfiylik siyosati</h1>
<p class="meta">O'zbekcha · <a href="#ru">Русский</a> · <a href="#en">English</a> · Oxirgi yangilanish: 2026-11-15</p>

<section id="uz">
<h2>1. Biz kimmiz</h2>
<p>PROSTAFF — mijozlarni ishchilar va xizmat ko'rsatuvchi firmalar bilan bog'laydigan platforma. Ma'lumotlaringizni PROSTAFF qayta ishlaydi. Murojaat: telefon <b>+998 94 080 94 45</b>, Telegram <b>@Prostaffsupport</b> (ish kunlari 9:00–18:00). Foydalanish shartlari ilovadagi «Ommaviy oferta»da.</p>

<h2>2. Qanday ma'lumotlar yig'iladi</h2>
<ul>
<li><b>Hamma foydalanuvchidan:</b> ism, telefon raqami, parol (faqat shifrlangan ko'rinishda), profil rasmi (ixtiyoriy), e-mail (kiritilgan bo'lsa), qurilmaning bildirishnoma tokeni.</li>
<li><b>Mijozdan:</b> buyurtma matni, rasmlari, narxi va manzili (xaritadagi nuqta), telefon raqami va ismi, qoldirgan baho va sharhlari.</li>
<li><b>Ishchidan:</b> to'liq ism, jinsi, yoshi, narxlar oralig'i, kategoriyalari, o'zi haqida matn, tajriba, manzil, rasmlar, <b>passport seriyasi</b> (bepul buyurtmalar hisobini yuritish va takroriy ro'yxatdan o'tishning oldini olish uchun) va <b>joylashuvi</b>.</li>
<li><b>Firmadan:</b> nomi, STIR (INN), telefon, tavsif, manzil, rasmlar, obuna ma'lumotlari va (yoqilgan bo'lsa) joriy joylashuvi.</li>
<li><b>To'lovlar:</b> to'lov summasi, holati va identifikatorlari. <b>Karta raqami, muddati va CVV kodi serverlarimizda saqlanmaydi:</b> ular to'lovni amalga oshirish uchun to'lov tizimiga (Atmos) uzatiladi.</li>
</ul>

<h2>3. Joylashuv</h2>
<ul>
<li><b>Mijoz:</b> yaqin atrofdagi ishchi va firmalarni ko'rsatish uchun joylashuvi so'raladi. Buyurtma manzili buyurtma bilan birga saqlanadi.</li>
<li><b>Ishchi:</b> «Onlayn» holatida bo'lgan vaqtda joylashuvi taxminan har daqiqada yoki 150 metr siljiganda serverga yuboriladi, ilova yopiq yoki boshqa ilova ochiq bo'lganda ham. Bu yaqin atrofdagi buyurtmalarni to'g'ri yuborish uchun kerak. Faqat oxirgi ma'lum joy saqlanadi, harakat tarixi yig'ilmaydi. «Oflayn» bo'lganda yuborish to'xtaydi.</li>
<li><b>Firma:</b> faqat «Jonli joylashuv» yoqilgan bo'lsagina, onlayn vaqtida.</li>
<li>Joylashuvga ruxsatni telefon sozlamalaridan istalgan vaqt olib qo'yishingiz mumkin; shunda ishchi buyurtma ololmaydi.</li>
</ul>

<h2>4. Nima uchun ishlatiladi</h2>
<ul>
<li>akkaunt ochish, kirish va parolni tiklash (SMS kod yuboriladi);</li>
<li>buyurtmani mos ishchi yoki firmaga yetkazish, ular bilan aloqa qilish imkonini berish;</li>
<li>to'lov, hamyon va obuna hisobi;</li>
<li>yangi buyurtma va holat o'zgarishi haqida bildirishnoma yuborish;</li>
<li>baholar va reytingni yuritish, firibgarlik va suiiste'molning oldini olish, texnik nosozliklarni topish.</li>
</ul>
<p>Ma'lumotlar sotilmaydi, reklama va kuzatuv (tracking) uchun ishlatilmaydi.</p>

<h2>5. Ma'lumotlar kimga beriladi</h2>
<ul>
<li><b>Buyurtma tomonlariga:</b> ishchi buyurtmani qabul qilgach mijozning telefon raqami va manzilini, mijoz esa ishchining ismi, telefoni, rasmi va bahosini ko'radi.</li>
<li><b>Xizmat ko'rsatuvchilarga (ular bizning nomimizdan ishlaydi):</b> SMS yuborish — Eskiz; bildirishnomalar — Google Firebase Cloud Messaging va Expo; xarita ko'rsatish — Yandex Maps (xaritadagi nuqta koordinatalari); to'lov — Atmos.</li>
<li>Qonun talab qilgan hollarda vakolatli organlarga.</li>
</ul>

<h2>6. Qancha vaqt saqlanadi</h2>
<ul>
<li>Akkaunt ma'lumotlari — akkaunt o'chirilguncha.</li>
<li>Buyurtma yopilgach, unga biriktirilgan rasmlar odatda o'chiriladi. Buyurtmalar tarixi va to'lov yozuvlari hisob-kitob va qonun talablari uchun saqlanadi.</li>
<li>Texnik jurnallarda (so'rov vaqti, manzili, IP) ma'lumotlar cheklangan muddat saqlanadi.</li>
</ul>

<h2>7. Sizning huquqlaringiz</h2>
<ul>
<li>Ma'lumotlaringizni ilovada ko'rish va yangilash (Profil).</li>
<li><b>Akkauntni o'chirish:</b> Profil → «Akkauntni o'chirish». Nima o'chishi va nima saqlanishi <a href="/delete-account">shu sahifada</a> yozilgan.</li>
<li>Ruxsatlarni (joylashuv, bildirishnoma, rasmlar) telefon sozlamalaridan olib qo'yish.</li>
<li>Savol yoki so'rov bilan yuqoridagi aloqa orqali murojaat qilish.</li>
</ul>

<h2>8. Xavfsizlik va bolalar</h2>
<p>Ma'lumotlar shifrlangan (HTTPS) ulanish orqali uzatiladi, parollar shifrlangan holda saqlanadi, kirish tokenlari telefonning xavfsiz xotirasida turadi. Platforma 18 yoshga to'lmaganlar uchun mo'ljallanmagan.</p>

<h2>9. O'zgarishlar</h2>
<p>Siyosat o'zgarsa, shu sahifa yangilanadi va yuqoridagi sana o'zgaradi.</p>
</section>

<section id="ru" lang="ru">
<h2>Политика конфиденциальности (кратко, по-русски)</h2>
<p><b>Оператор:</b> PROSTAFF, телефон +998 94 080 94 45, Telegram @Prostaffsupport.</p>
<p><b>Что собираем:</b> имя, телефон, пароль (в зашифрованном виде), фото профиля, e-mail (если указан), токен уведомлений; у заказчика — описание, фото, цену и адрес заказа, отзывы; у исполнителя — ФИО, пол, возраст, цены, категории, описание, опыт, адрес, фото, <b>серию паспорта</b> (учёт бесплатных заказов, защита от повторной регистрации) и <b>местоположение</b>; у фирмы — название, ИНН, телефон, адрес, фото, подписку. Данные карты (номер, срок, CVV) <b>не хранятся</b> на наших серверах, а передаются платёжной системе Atmos.</p>
<p><b>Местоположение:</b> у заказчика — для показа ближайших исполнителей; у исполнителя — пока он «Онлайн», примерно раз в минуту или каждые 150 м, в том числе когда приложение закрыто или открыто другое; хранится только последнее известное положение. При выходе «Офлайн» передача прекращается.</p>
<p><b>Зачем:</b> регистрация и вход (SMS), доставка заказов подходящим исполнителям, платежи и кошелёк, уведомления, рейтинги, защита от злоупотреблений. Данные не продаются и не используются для рекламы или отслеживания.</p>
<p><b>Кому передаются:</b> сторонам заказа (контакты после принятия заказа); поставщикам, работающим от нашего имени: Eskiz (SMS), Google Firebase Cloud Messaging и Expo (уведомления), Yandex Maps (отображение карты), Atmos (платежи); органам власти — по закону.</p>
<p><b>Срок хранения:</b> данные аккаунта — до удаления; фото заказа обычно удаляются после его завершения; история заказов и платёжные записи хранятся для учёта и по закону.</p>
<p><b>Ваши права:</b> просматривать и обновлять данные в профиле; <b>удалить аккаунт</b> (Профиль → «Удалить аккаунт», подробности на странице <a href="/delete-account">/delete-account</a>); отозвать разрешения в настройках телефона. Приложение не предназначено для лиц младше 18 лет.</p>
</section>

<section id="en" lang="en">
<h2>Privacy policy (summary in English)</h2>
<p><b>Operator:</b> PROSTAFF, phone +998 94 080 94 45, Telegram @Prostaffsupport.</p>
<p><b>What we collect:</b> name, phone number, password (stored hashed), profile photo, e-mail (if given), notification token; from customers the order text, photos, price and address, and reviews; from workers full name, gender, age, prices, categories, description, experience, address, photos, <b>passport series</b> (to count free orders and stop repeat sign-ups) and <b>location</b>; from firms name, tax ID, phone, address, photos and subscription. Card details (number, expiry, CVV) are <b>not stored</b> on our servers; they are passed to the payment provider, Atmos.</p>
<p><b>Location:</b> for customers, to show nearby workers; for workers, while they are “Online”, about once a minute or every 150 m, including when the app is closed or another app is open; only the last known position is kept. Going “Offline” stops it.</p>
<p><b>Why:</b> sign-up and sign-in (SMS), delivering orders to suitable workers, payments and wallet, notifications, ratings, preventing abuse. Data is not sold and not used for advertising or tracking.</p>
<p><b>Who receives it:</b> the other party to an order (contact details once an order is accepted); providers working on our behalf: Eskiz (SMS), Google Firebase Cloud Messaging and Expo (notifications), Yandex Maps (map display), Atmos (payments); authorities where the law requires.</p>
<p><b>Retention:</b> account data until the account is deleted; order photos are normally deleted when the order closes; order history and payment records are kept for accounting and legal reasons.</p>
<p><b>Your rights:</b> view and update your data in Profile; <b>delete your account</b> (Profile → “Delete account”, details at <a href="/delete-account">/delete-account</a>); withdraw permissions in phone settings. The app is not intended for people under 18.</p>
</section>
</main></body></html>`
