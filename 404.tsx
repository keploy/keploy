import Footer from '../components/footer'; // Adjust path if needed

import { useEffect } from "react";
import { useRouter } from "next/router";
import Head from "next/head";
import NotFoundPage from "../components/NotFoundPage";

export default function Custom404() {
  const router = useRouter();
  const asPath = router.asPath;

  useEffect(() => {
    const redirectTimeout = setTimeout(() => {
      if (asPath.startsWith("/community/")) {
        router.replace("/community");
      } else if (asPath.startsWith("/technology/")) {
        router.replace("/technology");
      } else {
        router.replace("/");
      }
    }, 3000); 

    return () => clearTimeout(redirectTimeout);
  }, [asPath, router]);

  return (
    <>
      <Head>
        <title>404 - Page Not Found | Keploy Blog</title>
      </Head>
      <NotFoundPage />
      <div style={{ textAlign: "center", marginTop: "20px" }}>
        <p>You will be redirected shortly...</p>
      </div>
      <Footer />
    </>
  );
}