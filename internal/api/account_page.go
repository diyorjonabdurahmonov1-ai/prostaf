package api

import "net/http"

// GET /delete-account — what Google Play and the App Store ask for: a public
// page that says how to delete an account and what that does to the data.
func (s *Server) handleDeleteAccountPage(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "public, max-age=3600")
	_, _ = w.Write([]byte(accountDeletionHTML))
}

const accountDeletionHTML = `<!doctype html>
<html lang="uz">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>PROSTAFF — akkauntni o'chirish</title>
<style>
  body{font:16px/1.55 system-ui,-apple-system,Segoe UI,Roboto,sans-serif;margin:0;background:#f6f7f9;color:#14181f}
  main{max-width:720px;margin:0 auto;padding:28px 20px 60px}
  h1{font-size:26px;margin:0 0 6px} h2{font-size:18px;margin:28px 0 8px}
  section{background:#fff;border-radius:14px;padding:18px 20px;margin:14px 0;box-shadow:0 1px 3px rgba(0,0,0,.06)}
  ol,ul{padding-left:22px;margin:6px 0} li{margin:4px 0} .lang{color:#5b6472;font-size:14px}
</style>
</head>
<body><main>
<h1>PROSTAFF — akkauntni o'chirish</h1>
<p class="lang">O'zbekcha · Русский · English</p>

<section>
<h2>Qanday o'chiriladi</h2>
<ol>
<li>PROSTAFF ilovasiga kiring.</li>
<li><b>Profil</b> bo'limini oching.</li>
<li>Pastdagi <b>«Akkauntni o'chirish»</b> tugmasini bosing va tasdiqlang.</li>
</ol>
<p>Ilovaga kira olmasangiz: «Parolni unutdim» orqali SMS kod bilan kirib, shu amalni bajaring.</p>
<p>Tugallanmagan buyurtmangiz bo'lsa, avval uni yakunlang yoki bekor qiling.</p>
</section>

<section>
<h2>Nima o'chiriladi</h2>
<ul>
<li>Ism, telefon raqami, e-mail, rasm, parol va barcha kirish sessiyalari;</li>
<li>ishchi yoki firma profili, passport ma'lumotlari va yuklangan barcha rasmlar;</li>
<li>bildirishnomalar, buyurtmalardagi telefon raqami va ismingiz, yozgan sharhlaringiz matni.</li>
</ul>
<h2>Nima saqlanadi</h2>
<ul>
<li>Buyurtmalar tarixi va to'lov hamda hamyon yozuvlari, ularda sizni aniqlaydigan ma'lumot qolmaydi (qonun va hisob-kitob talablari uchun);</li>
<li>boshqa foydalanuvchilar sizga qo'ygan yulduzli baholar;</li>
<li>bepul buyurtmalar hisobi (passport bo'yicha), bepul ishlarni qayta olish uchun akkaunt ochib-o'chirib bo'lmasligi uchun.</li>
</ul>
</section>

<section lang="ru">
<h2>Как удалить аккаунт</h2>
<p>Откройте приложение PROSTAFF → <b>Профиль</b> → <b>«Удалить аккаунт»</b> и подтвердите. Если у вас есть незавершённый заказ, сначала завершите или отмените его. Если вы не можете войти — восстановите пароль через SMS и удалите аккаунт.</p>
<p><b>Удаляется:</b> имя, телефон, e-mail, фото, пароль, сессии, профиль исполнителя или фирмы, паспортные данные, загруженные фото, уведомления, ваши номер и имя в заказах, текст оставленных отзывов.
<b>Сохраняется без персональных данных:</b> история заказов, платёжные и кошелёк-записи (требования учёта и закона), звёздные оценки от других, счётчик бесплатных заказов по паспорту.</p>
</section>

<section lang="en">
<h2>How to delete your account</h2>
<p>Open the PROSTAFF app → <b>Profile</b> → <b>“Delete account”</b> and confirm. If you have an unfinished order, finish or cancel it first. If you cannot sign in, reset your password by SMS and then delete the account.</p>
<p><b>Deleted:</b> name, phone number, e-mail, photo, password, sessions, worker or firm profile, passport details, uploaded photos, notifications, your name and number on orders, the text of reviews you wrote.
<b>Kept without personal data:</b> order history, payment and wallet records (accounting and legal requirements), star ratings given by others, and the free-order counter tied to the passport.</p>
</section>
</main></body></html>`
