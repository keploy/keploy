<p align="center">
  <img align="center" src="https://docs.keploy.io/img/keploy-logo-dark.svg?s=200&v=4" height="40%" width="40%"  alt="keploy logo"/>
</p>
<h3 align="center">
<b>
⚡️ API-тесты быстрее модульных, на основе пользовательского трафика ⚡️
</b>
</h3 >
<p align="center">
🌟 Необходимый инструмент для разработчиков в эпоху ИИ 🌟
</p>

---

<h4 align="center">

<a href="https://x.com/Keployio">
    <img src="https://img.shields.io/badge/follow-%40keployio-1DA1F2?logo=X&style=social" alt="Keploy X!" />
  </a>

<a href="https://github.com/Keploy/Keploy/">
   <img src="https://img.shields.io/github/stars/keploy/keploy?color=%23EAC54F&logo=github&label=Help%20us%20reach%2020K%20stars!%20Now%20at:" alt="Help us reach 20k stars!" />
</a>

<a href="https://landscape.cncf.io/?item=app-definition-and-development--continuous-integration-delivery--keploy">
    <img src="https://img.shields.io/badge/CNCF%20Landscape-5699C6?logo=cncf&style=social" alt="Keploy CNCF Landscape" />
  </a>

[![Slack](https://img.shields.io/badge/Slack-4A154B?style=for-the-badge&logo=slack&logoColor=white)](https://join.slack.com/t/keploy/shared_invite/zt-357qqm9b5-PbZRVu3Yt2rJIa6ofrwWNg)
[![LinkedIn](https://img.shields.io/badge/linkedin-%230077B5.svg?style=for-the-badge&logo=linkedin&logoColor=white)](https://www.linkedin.com/company/keploy/)
[![YouTube](https://img.shields.io/badge/YouTube-%23FF0000.svg?style=for-the-badge&logo=YouTube&logoColor=white)](https://www.youtube.com/channel/UC6OTg7F4o0WkmNtSoob34lg)
[![X](https://img.shields.io/badge/X-%231DA1F2.svg?style=for-the-badge&logo=X&logoColor=white)](https://x.com/Keployio)

<a href="https://trendshift.io/repositories/3262" target="_blank"><img src="https://trendshift.io/api/badge/repositories/3262" alt="keploy%2Fkeploy | Trendshift" style="width: 250px; height: 55px;" width="250" height="55"/></a>

</h4>

[Keploy](https://keploy.io) — это **ориентированный на разработчиков** инструмент для тестирования API, который создает **тесты вместе со встроенными моками** быстрее, чем модульные тесты.

Keploy не только записывает вызовы API, но также фиксирует обращения к базе данных и воспроизводит их во время тестирования, что делает его **простым в использовании, мощным и расширяемым**.

<img src="https://raw.githubusercontent.com/keploy/docs/main/static/gif/record-tc.gif" width="60%" alt="Convert API calls to test cases"/>

> 🐰 **Интересный факт:** Keploy использует сам себя для тестирования! Посмотрите наш стильный бейдж покрытия: [![Coverage Status](https://coveralls.io/repos/github/keploy/keploy/badge.svg?branch=main&kill_cache=1)](https://coveralls.io/github/keploy/keploy?branch=main&kill_cache=1) &nbsp;

## 🚨 Вы здесь ради [Генератора юнит-тестов](README-UnitGen.md) (ut-gen)?

Keploy недавно запустил первую в мире реализацию генератора юнит-тестов (ut-gen) на основе [исследовательской работы Meta LLM](https://arxiv.org/pdf/2402.09171). Он понимает семантику кода и генерирует осмысленные юнит-тесты, преследуя следующие цели:

- **Автоматизировать генерацию юнит-тестов (UTG)**: Быстро создавать комплексные юнит-тесты и сокращать рутинные ручные усилия.

- **Улучшить обработку граничных случаев**: Расширять и улучшать охват автоматизированных тестов для более сложных сценариев, часто упускаемых вручную.

- **Повысить покрытие тестами**: По мере роста кодовой базы обеспечение исчерпывающего покрытия должно становиться осуществимым, что соответствует нашей миссии.

### 📜 Следуйте [README Генератора юнит-тестов](README-UnitGen.md)! ✅

## 📘 Документация!

Станьте профессионалом Keploy с **[Документацией Keploy](https://keploy.io/docs/)**.

<img src="https://raw.githubusercontent.com/keploy/docs/main/static/gif/record-replay.gif" width="100%" alt="Record Replay Testing"/>

# 🚀 Быстрая установка (генератор API-тестов)

Интегрируйте Keploy, установив агент локально. Изменения кода не требуются.

```shell
curl --silent -O -L https://keploy.io/install.sh && source install.sh
```

##  🎬 Запись тест-кейсов

Запустите ваше приложение с Keploy, чтобы преобразовать API-вызовы в тесты и моки/стабы.

```zsh
keploy record -c "CMD_TO_RUN_APP" 
```

Например, если вы используете простое Python-приложение, `CMD_TO_RUN_APP` будет выглядеть как `python main.py`, для Golang — `go run main.go`, для Java — `java -jar xyz.jar`, для Node — `npm start`.

```zsh
keploy record -c "python main.py"
```

## 🧪 Запуск тестов

Отключите базы данных, Redis, Kafka или любые другие сервисы, которые использует ваше приложение. Keploy не нуждается в них во время тестирования.

```zsh
keploy test -c "CMD_TO_RUN_APP" --delay 10
```

## ✅ Интеграция покрытия тестов

Чтобы интегрироваться с вашей библиотекой модульного тестирования и увидеть комбинированное покрытие тестов, следуйте этому [руководству по покрытию тестов](https://keploy.io/docs/server/sdk-installation/go/).

> ####  **Если вам понравилось:** Пожалуйста, поставьте 🌟 звезду этому репозиторию! Это бесплатно и вызовет улыбку. 😄 👏

## Настройка в один клик 🚀

Быстро настройте и запустите Keploy без установки на локальную машину:

[![GitHub Codescape](https://img.shields.io/badge/GH%20codespace-3670A0?style=for-the-badge&logo=github&logoColor=fff)]([https://github.dev/Sonichigo/mux-sql](https://github.dev/Sonichigo/mux-sql))

## 🤔 Вопросы?

Свяжитесь с нами. Мы здесь, чтобы помочь!

[![Slack](https://img.shields.io/badge/Slack-4A154B?style=for-the-badge&logo=slack&logoColor=white)](https://join.slack.com/t/keploy/shared_invite/zt-357qqm9b5-PbZRVu3Yt2rJIa6ofrwWNg)
[![LinkedIn](https://img.shields.io/badge/linkedin-%230077B5.svg?style=for-the-badge&logo=linkedin&logoColor=white)](https://www.linkedin.com/company/keploy/)
[![YouTube](https://img.shields.io/badge/YouTube-%23FF0000.svg?style=for-the-badge&logo=YouTube&logoColor=white)](https://www.youtube.com/channel/UC6OTg7F4o0WkmNtSoob34lg)
[![X](https://img.shields.io/badge/X-%231DA1F2.svg?style=for-the-badge&logo=X&logoColor=white)](https://x.com/Keployio)

## 🌐 Поддержка языков

От гофера Go 🐹 до змеи Python 🐍 — мы поддерживаем:

![Go](https://img.shields.io/badge/go-%2300ADD8.svg?style=for-the-badge&logo=go&logoColor=white)
![Java](https://img.shields.io/badge/java-%23ED8B00.svg?style=for-the-badge&logo=java&logoColor=white)
![NodeJS](https://img.shields.io/badge/node.js-6DA55F?style=for-the-badge&logo=node.js&logoColor=white)
![Rust](https://img.shields.io/badge/Rust-darkred?style=for-the-badge&logo=rust&logoColor=white)
![C#](https://img.shields.io/badge/csharp-purple?style=for-the-badge&logo=csharp&logoColor=white)
![Python](https://img.shields.io/badge/python-3670A0?style=for-the-badge&logo=python&logoColor=ffdd54)

## 🫰 Пользователи Keploy 🧡

Итак, вы и ваша организация используете Keploy? Это замечательно. Пожалуйста, добавьте себя в [**этот список,**](https://github.com/orgs/keploy/discussions/1765), и мы отправим вам подарки! 💖

Мы рады и гордимся тем, что вы все стали частью нашего сообщества! 💖

## 🎩 Как происходит магия?

Keploy proxy захватывает и воспроизводит **ВСЕ** (CRUD-операции, включая неидемпотентные API) сетевые взаимодействия вашего приложения.

Отправьтесь в путешествие по **[How Keploy Works?](https://keploy.io/docs/keploy-explained/how-keploy-works/)**, чтобы раскрыть секреты за кулисами!

## 🔧 Основные возможности

- ♻️ **Комбинированное покрытие тестов:** Объедините тесты Keploy с вашими любимыми библиотеками тестирования (JUnit, go-test, py-test, jest), чтобы увидеть общее покрытие тестов.


- 🤖 **Инструментирование EBPF:** Keploy использует EBPF как секретный ингредиент, чтобы сделать интеграцию бескодовой, независимой от языка и очень легковесной.


- 🌐 **Интеграция с CI/CD:** Запускайте тесты с моками где угодно — локально через CLI, в вашем CI-пайплайне (Jenkins, Github Actions...) или даже в Kubernetes-кластере.


- 📽️ **Запись и воспроизведение сложных сценариев:** Keploy может записывать и воспроизводить сложные, распределенные API-сценарии в виде моков и стабов. Это как машина времени для ваших тестов — экономит кучу времени!


- 🎭 **Многоцелевые моки:** Вы также можете использовать моки, сгенерированные Keploy, в качестве серверных тестов!

👉 **Исходный код на GitHub**: [github.com/keploy/keploy](https://github.com/keploy/keploy)

## 👨🏻‍💻 Давайте строить вместе! 👩🏻‍💻

Неважно, новичок вы в программировании или опытный разработчик 🧙‍♀️ — ваше мнение бесценно. Ознакомьтесь с нашими материалами:

📜 [Руководство по внесению вклада](https://github.com/keploy/keploy/blob/main/CONTRIBUTING.md)

❤️ [Кодекс поведения](https://github.com/keploy/keploy/blob/main/CODE_OF_CONDUCT.md)

## 🐲 Текущие ограничения!

- **Модульное тестирование:** Хотя Keploy разработан для работы вместе с фреймворками модульного тестирования (Go test, JUnit...) и может увеличивать общее покрытие кода тестами, он всё же генерирует интеграционные тесты.
- **Продуктивные среды:** В настоящее время Keploy ориентирован на генерацию тестов для разработчиков. Эти тесты можно записывать в любой среде, но мы не тестировали его в высоконагруженных продуктивных средах. Для этого потребуется надёжная система дедупликации, чтобы избежать записи слишком большого количества избыточных тестов. У нас есть идеи по созданию такой системы [#27](https://github.com/keploy/keploy/issues/27)

## ✨ Ресурсы!

🤔 [Часто задаваемые вопросы](https://keploy.io/docs/keploy-explained/faq/)

🕵️‍️ [Почему Keploy](https://keploy.io/docs/keploy-explained/why-keploy/)

⚙️ [Руководство по установке](https://keploy.io/docs/application-development/)

📖 [Руководство по внесению вклада](https://keploy.io/docs/keploy-explained/contribution-guide/)